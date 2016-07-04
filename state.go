// Copyright 2016 Mender Software AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package main

import (
	"io"
	"time"

	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
)

type State interface {
	// Perform state action, returns next state and boolean flag indicating if
	// execution was cancelled or not
	Handle(c Controller) (State, bool)
	// Cancel state action, returns true if action was cancelled
	Cancel() bool
	// Return numeric state ID
	Id() MenderState
}

type StateRunner interface {
	// Set runner's state to 's'
	SetState(s State)
	// Obtain runner's state
	GetState() State
	// TODO generic state run action
}

var (
	initState = &InitState{
		BaseState{
			id: MenderStateInit,
		},
	}

	bootstrappedState = &BootstrappedState{
		BaseState{
			id: MenderStateBootstrapped,
		},
	}

	authorizeWaitState = NewAuthorizeWaitState()

	authorizedState = &AuthorizedState{
		BaseState{
			id: MenderStateAuthorized,
		},
	}

	updateCheckWaitState = NewUpdateCheckWaitState()

	updateCheckState = &UpdateCheckState{
		BaseState{
			id: MenderStateUpdateCheck,
		},
	}

	doneState = &FinalState{
		BaseState{
			id: MenderStateDone,
		},
	}
)

// Helper base state with some convenience methods
type BaseState struct {
	id MenderState
}

func (b *BaseState) Id() MenderState {
	return b.id
}

func (b *BaseState) Cancel() bool {
	return false
}

type CancellableState struct {
	BaseState
	cancel chan bool
}

func NewCancellableState(base BaseState) CancellableState {
	return CancellableState{
		base,
		make(chan bool),
	}
}

func (cs *CancellableState) StateAfterWait(next, same State, wait time.Duration) (State, bool) {
	ticker := time.NewTicker(wait)

	defer ticker.Stop()
	select {
	case <-ticker.C:
		log.Debugf("wait complete")
		return next, false
	case <-cs.cancel:
		log.Infof("wait canceled")
	}

	return same, true
}

func (cs *CancellableState) Cancel() bool {
	cs.cancel <- true
	return true
}

func (cs *CancellableState) Stop() {
	close(cs.cancel)
}

type InitState struct {
	BaseState
}

func (i *InitState) Handle(c Controller) (State, bool) {
	log.Debugf("handle init state")
	if err := c.Bootstrap(); err != nil {
		log.Errorf("bootstrap failed: %s", err)
		return NewErrorState(err), false
	}
	return bootstrappedState, false
}

type BootstrappedState struct {
	BaseState
}

func (b *BootstrappedState) Handle(c Controller) (State, bool) {
	log.Debugf("handle bootstrapped state")
	if err := c.Authorize(); err != nil {
		log.Errorf("authorize failed: %v", err)
		if !err.IsFatal() {
			return authorizeWaitState, false
		} else {
			return NewErrorState(err), false
		}
	}

	return authorizedState, false
}

type UpdateCommitState struct {
	BaseState
	update UpdateResponse
}

func NewUpdateCommitState(update UpdateResponse) State {
	return &UpdateCommitState{
		BaseState{
			id: MenderStateUpdateCommit,
		},
		update,
	}
}

func (uc *UpdateCommitState) Handle(c Controller) (State, bool) {
	log.Debugf("handle update commit state")
	err := c.CommitUpdate()
	if err != nil {
		log.Errorf("update commit failed: %s", err)
		return NewErrorState(NewFatalError(err)), false
	}

	if merr := c.ReportUpdateStatus(uc.update, statusSuccess); merr != nil {
		log.Errorf("failed to report success status: %v", err)
		// TODO: until backend has implemented status reporting, this error cannot
		// result in update being aborted. Once required API endpoint is available
		// commend the line below and remove this comment.

		// return NewUpdateErrorState(merr, uc.update), false
	}

	// done?
	return updateCheckWaitState, false
}

type UpdateCheckState struct {
	BaseState
}

func (u *UpdateCheckState) Handle(c Controller) (State, bool) {
	log.Debugf("handle update check state")
	update, err := c.CheckUpdate()
	if err != nil {
		log.Errorf("update check failed: %s", err)
		// maybe transient error?
		return NewErrorState(err), false
	}

	if update != nil {
		// TODO: save update information state

		// custom state data?
		return NewUpdateFetchState(*update), false
	}

	return updateCheckWaitState, false
}

type UpdateFetchState struct {
	BaseState
	update UpdateResponse
}

func NewUpdateFetchState(update UpdateResponse) State {
	return &UpdateFetchState{
		BaseState{
			id: MenderStateUpdateFetch,
		},
		update,
	}
}

func (u *UpdateFetchState) Handle(c Controller) (State, bool) {
	// report downloading, don't care about errors
	c.ReportUpdateStatus(u.update, statusDownloading)

	log.Debugf("handle update fetch state")
	in, size, err := c.FetchUpdate(u.update.Image.URI)
	if err != nil {
		log.Errorf("update fetch failed: %s", err)
		return NewUpdateErrorState(NewTransientError(err), u.update), false
	}

	return NewUpdateInstallState(in, size, u.update), false
}

type UpdateInstallState struct {
	BaseState
	// reader for obtaining image data
	imagein io.ReadCloser
	// expected image size
	size   int64
	update UpdateResponse
}

func NewUpdateInstallState(in io.ReadCloser, size int64, update UpdateResponse) State {
	return &UpdateInstallState{
		BaseState{
			id: MenderStateUpdateInstall,
		},
		in,
		size,
		update,
	}
}

func (u *UpdateInstallState) Handle(c Controller) (State, bool) {
	// report installing, don't care about errors
	c.ReportUpdateStatus(u.update, statusInstalling)

	log.Debugf("handle update install state")
	if err := c.InstallUpdate(u.imagein, u.size); err != nil {
		log.Errorf("update install failed: %s", err)
		return NewUpdateErrorState(NewTransientError(err), u.update), false
	}

	if err := c.EnableUpdatedPartition(); err != nil {
		log.Errorf("enabling updated partition failed: %s", err)
		return NewUpdateErrorState(NewTransientError(err), u.update), false
	}

	return NewRebootState(u.update), false
}

type UpdateCheckWaitState struct {
	CancellableState
}

func NewUpdateCheckWaitState() State {
	return &UpdateCheckWaitState{
		NewCancellableState(BaseState{
			id: MenderStateUpdateCheckWait,
		}),
	}
}

func (u *UpdateCheckWaitState) Handle(c Controller) (State, bool) {
	log.Debugf("handle update check wait state")

	intvl := c.GetUpdatePollInterval()

	log.Debugf("wait %v before next poll", intvl)
	return u.StateAfterWait(updateCheckState, u, intvl)
}

// Cancel wait state
func (u *UpdateCheckWaitState) Cancel() bool {
	u.cancel <- true
	return true
}

type AuthorizeWaitState struct {
	CancellableState
}

func NewAuthorizeWaitState() State {
	return &AuthorizeWaitState{
		NewCancellableState(BaseState{
			id: MenderStateAuthorizeWait,
		}),
	}
}

func (a *AuthorizeWaitState) Handle(c Controller) (State, bool) {
	log.Debugf("handle authorize wait state")
	intvl := c.GetUpdatePollInterval()

	log.Debugf("wait %v before next authorization attempt", intvl)
	return a.StateAfterWait(bootstrappedState, a, intvl)
}

type AuthorizedState struct {
	BaseState
}

func (a *AuthorizedState) Handle(c Controller) (State, bool) {
	// TODO HasUpgrade should return update information
	has, err := c.HasUpgrade()
	if err != nil {
		log.Errorf("has upgrade check failed: %s", err)
		// we may or may now have an upddate ready
		return NewErrorState(err), false
	}
	if has {
		// TODO restore update information
		return NewUpdateCommitState(UpdateResponse{}), false
	}

	return updateCheckWaitState, false
}

type ErrorState struct {
	BaseState
	cause menderError
}

func NewErrorState(err menderError) State {
	if err == nil {
		err = NewFatalError(errors.New("general error"))
	}

	return &ErrorState{
		BaseState{
			id: MenderStateError,
		},
		err,
	}
}

func (e *ErrorState) Handle(c Controller) (State, bool) {
	log.Infof("handling error state, current error: %v", e.cause.Error())
	// decide if error is transient, exit for now
	if e.cause.IsFatal() {
		return doneState, false
	}
	return initState, false
}

func (e *ErrorState) IsFatal() bool {
	return e.cause.IsFatal()
}

type UpdateErrorState struct {
	ErrorState
	update UpdateResponse
}

func NewUpdateErrorState(err menderError, update UpdateResponse) State {
	return &UpdateErrorState{
		ErrorState{
			BaseState{
				id: MenderStateUpdateError,
			},
			err,
		},
		update,
	}
}

func (ue *UpdateErrorState) Handle(c Controller) (State, bool) {
	// TODO error handling
	c.ReportUpdateStatus(ue.update, statusFailure)
	return initState, false
}

type RebootState struct {
	BaseState
	update UpdateResponse
}

func NewRebootState(update UpdateResponse) State {
	return &RebootState{
		BaseState{
			id: MenderStateReboot,
		},
		update,
	}
}

func (e *RebootState) Handle(c Controller) (State, bool) {
	c.ReportUpdateStatus(e.update, statusRebooting)

	log.Debugf("handle reboot state")
	if err := c.Reboot(); err != nil {
		return NewErrorState(NewFatalError(err)), false
	}
	return doneState, false
}

type FinalState struct {
	BaseState
}

func (f *FinalState) Handle(c Controller) (State, bool) {
	panic("reached final state")
}
