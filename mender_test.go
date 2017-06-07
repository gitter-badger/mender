// Copyright 2017 Northern.tech AS
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
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"syscall"
	"testing"
	"time"

	"github.com/mendersoftware/mender-artifact/artifact"
	"github.com/mendersoftware/mender-artifact/awriter"
	"github.com/mendersoftware/mender-artifact/handlers"
	"github.com/mendersoftware/mender/client"
	cltest "github.com/mendersoftware/mender/client/test"
	"github.com/mendersoftware/mender/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type testMenderPieces struct {
	MenderPieces
}

func Test_getArtifactName_noArtifactNameInFile_returnsEmptyName(t *testing.T) {
	mender := newDefaultTestMender()

	artifactInfoFile, _ := os.Create("artifact_info")
	defer os.Remove("artifact_info")

	fileContent := "dummy_data"
	artifactInfoFile.WriteString(fileContent)
	// rewind to the beginning of file
	//artifactInfoFile.Seek(0, 0)

	mender.artifactInfoFile = "artifact_info"

	artName, err := mender.GetCurrentArtifactName()
	assert.NoError(t, err)
	assert.Equal(t, "", artName)
}

func Test_getArtifactName_malformedArtifactNameLine_returnsError(t *testing.T) {
	mender := newDefaultTestMender()

	artifactInfoFile, _ := os.Create("artifact_info")
	defer os.Remove("artifact_info")

	fileContent := "artifact_name"
	artifactInfoFile.WriteString(fileContent)
	// rewind to the beginning of file
	//artifactInfoFile.Seek(0, 0)

	mender.artifactInfoFile = "artifact_info"

	artName, err := mender.GetCurrentArtifactName()
	assert.Error(t, err)
	assert.Equal(t, "", artName)
}

func Test_getArtifactName_haveArtifactName_returnsName(t *testing.T) {
	mender := newDefaultTestMender()

	artifactInfoFile, _ := os.Create("artifact_info")
	defer os.Remove("artifact_info")

	fileContent := "artifact_name=mender-image"
	artifactInfoFile.WriteString(fileContent)
	mender.artifactInfoFile = "artifact_info"

	artName, err := mender.GetCurrentArtifactName()
	assert.NoError(t, err)
	assert.Equal(t, "mender-image", artName)
}

func newTestMender(runner *testOSCalls, config menderConfig, pieces testMenderPieces) *mender {
	// fill out missing pieces

	if pieces.store == nil {
		pieces.store = store.NewMemStore()
	}

	if pieces.device == nil {
		pieces.device = &fakeDevice{}
	}

	if pieces.authMgr == nil {

		ks := store.NewKeystore(pieces.store, defaultKeyFile)

		cmdr := newTestOSCalls("mac=foobar", 0)
		pieces.authMgr = NewAuthManager(AuthManagerConfig{
			AuthDataStore: pieces.store,
			KeyStore:      ks,
			IdentitySource: &IdentityDataRunner{
				cmdr: &cmdr,
			},
		})
	}

	mender, _ := NewMender(config, pieces.MenderPieces)
	mender.stateScriptPath = ""

	return mender
}

func newDefaultTestMender() *mender {
	return newTestMender(nil, menderConfig{}, testMenderPieces{})
}

func Test_ForceBootstrap(t *testing.T) {
	// generate valid keys
	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)

	merr := mender.Bootstrap()
	assert.NoError(t, merr)

	kdataold, err := ms.ReadAll(defaultKeyFile)
	assert.NoError(t, err)
	assert.NotEmpty(t, kdataold)

	mender.ForceBootstrap()

	assert.True(t, mender.needsBootstrap())

	merr = mender.Bootstrap()
	assert.NoError(t, merr)

	// bootstrap should have generated a new key
	kdatanew, err := ms.ReadAll(defaultKeyFile)
	assert.NoError(t, err)
	assert.NotEmpty(t, kdatanew)
	// we should have a new key
	assert.NotEqual(t, kdatanew, kdataold)
}

func Test_Bootstrap(t *testing.T) {
	mender := newTestMender(nil,
		menderConfig{},
		testMenderPieces{},
	)

	assert.True(t, mender.needsBootstrap())

	assert.NoError(t, mender.Bootstrap())

	mam, _ := mender.authMgr.(*MenderAuthManager)
	k := store.NewKeystore(mam.store, defaultKeyFile)
	assert.NotNil(t, k)
	assert.NoError(t, k.Load())
}

func Test_BootstrappedHaveKeys(t *testing.T) {

	// generate valid keys
	ms := store.NewMemStore()
	k := store.NewKeystore(ms, defaultKeyFile)
	assert.NotNil(t, k)
	assert.NoError(t, k.Generate())
	assert.NoError(t, k.Save())

	mender := newTestMender(nil,
		menderConfig{},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)
	assert.NotNil(t, mender)
	mam, _ := mender.authMgr.(*MenderAuthManager)
	assert.Equal(t, ms, mam.keyStore.GetStore())
	assert.NotNil(t, mam.keyStore.GetPrivateKey())

	// subsequen bootstrap should not fail
	assert.NoError(t, mender.Bootstrap())
}

func Test_BootstrapError(t *testing.T) {

	ms := store.NewMemStore()

	ms.Disable(true)

	var mender *mender
	mender = newTestMender(nil, menderConfig{}, testMenderPieces{
		MenderPieces: MenderPieces{
			store: ms,
		},
	})
	// store is disabled, attempts to load keys when creating authMgr should have
	// failed
	assert.Nil(t, mender.authMgr)

	ms.Disable(false)
	mender = newTestMender(nil, menderConfig{}, testMenderPieces{
		MenderPieces: MenderPieces{
			store: ms,
		},
	})
	assert.NotNil(t, mender.authMgr)

	ms.ReadOnly(true)

	err := mender.Bootstrap()
	assert.Error(t, err)
}

func Test_CheckUpdateSimple(t *testing.T) {
	// create temp dir
	td, _ := ioutil.TempDir("", "mender-install-update-")
	defer os.RemoveAll(td)

	// prepare fake artifactInfo file
	artifactInfo := path.Join(td, "artifact_info")
	// prepare fake device type file
	deviceType := path.Join(td, "device_type")

	var mender *mender

	mender = newTestMender(nil, menderConfig{
		ServerURL: "bogusurl",
	}, testMenderPieces{})

	up, err := mender.CheckUpdate()
	assert.Error(t, err)
	assert.Nil(t, up)

	srv := cltest.NewClientTestServer()
	defer srv.Close()

	srv.Update.Has = true

	mender = newTestMender(nil,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{})
	mender.artifactInfoFile = artifactInfo
	mender.deviceTypeFile = deviceType

	srv.Update.Current = client.CurrentUpdate{
		Artifact:   "fake-id",
		DeviceType: "hammer",
	}

	// test server expects current update information, request should fail
	up, err = mender.CheckUpdate()
	assert.Error(t, err)
	assert.Nil(t, nil)

	// NOTE: manifest file data must match current update information expected by
	// the server
	ioutil.WriteFile(artifactInfo, []byte("artifact_name=fake-id\nDEVICE_TYPE=hammer"), 0600)
	ioutil.WriteFile(deviceType, []byte("device_type=hammer"), 0600)

	currID, sErr := mender.GetCurrentArtifactName()
	assert.NoError(t, sErr)
	assert.Equal(t, "fake-id", currID)
	// make artifact name same as current, will result in no updates being available
	srv.Update.Data.Artifact.ArtifactName = currID

	up, err = mender.CheckUpdate()
	assert.Equal(t, err, NewTransientError(os.ErrExist))
	assert.NotNil(t, up)

	// make artifact name different from current
	srv.Update.Data.Artifact.ArtifactName = currID + "-fake"
	srv.Update.Has = true
	up, err = mender.CheckUpdate()
	assert.NoError(t, err)
	assert.NotNil(t, up)
	assert.Equal(t, *up, srv.Update.Data)

	// pretend that we got 204 No Content from the server, i.e empty response body
	srv.Update.Has = false
	up, err = mender.CheckUpdate()
	assert.NoError(t, err)
	assert.Nil(t, up)
}

func TestMenderHasUpgrade(t *testing.T) {
	mender := newTestMender(nil, menderConfig{}, testMenderPieces{
		MenderPieces: MenderPieces{
			device: &fakeDevice{
				retHasUpdate: true,
			},
		},
	})

	h, err := mender.HasUpgrade()
	assert.NoError(t, err)
	assert.True(t, h)

	mender = newTestMender(nil, menderConfig{}, testMenderPieces{
		MenderPieces: MenderPieces{
			device: &fakeDevice{
				retHasUpdate: false,
			},
		},
	})

	h, err = mender.HasUpgrade()
	assert.NoError(t, err)
	assert.False(t, h)

	mender = newTestMender(nil, menderConfig{}, testMenderPieces{
		MenderPieces: MenderPieces{
			device: &fakeDevice{
				retHasUpdateError: errors.New("failed"),
			},
		},
	})
	h, err = mender.HasUpgrade()
	assert.Error(t, err)
}

func TestMenderGetUpdatePollInterval(t *testing.T) {
	mender := newTestMender(nil, menderConfig{
		UpdatePollIntervalSeconds: 20,
	}, testMenderPieces{})

	intvl := mender.GetUpdatePollInterval()
	assert.Equal(t, time.Duration(20)*time.Second, intvl)
}

func TestMenderGetInventoryPollInterval(t *testing.T) {
	mender := newTestMender(nil, menderConfig{
		InventoryPollIntervalSeconds: 10,
	}, testMenderPieces{})

	intvl := mender.GetInventoryPollInterval()
	assert.Equal(t, time.Duration(10)*time.Second, intvl)
}

type testAuthDataMessenger struct {
	reqData  []byte
	sigData  []byte
	code     client.AuthToken
	reqError error
	rspError error
	rspData  []byte
}

func (t *testAuthDataMessenger) MakeAuthRequest() (*client.AuthRequest, error) {
	return &client.AuthRequest{
		Data:      t.reqData,
		Token:     t.code,
		Signature: t.sigData,
	}, t.reqError
}

func (t *testAuthDataMessenger) RecvAuthResponse(data []byte) error {
	t.rspData = data
	return t.rspError
}

type testAuthManager struct {
	authorized     bool
	authtoken      client.AuthToken
	authtokenErr   error
	haskey         bool
	generatekeyErr error
	testAuthDataMessenger
}

func (a *testAuthManager) IsAuthorized() bool {
	return a.authorized
}

func (a *testAuthManager) AuthToken() (client.AuthToken, error) {
	return a.authtoken, a.authtokenErr
}

func (a *testAuthManager) HasKey() bool {
	return a.haskey
}

func (a *testAuthManager) GenerateKey() error {
	return a.generatekeyErr
}

func (a *testAuthManager) RemoveAuthToken() error {
	return nil
}

func TestMenderAuthorize(t *testing.T) {
	runner := newTestOSCalls("", -1)

	rspdata := []byte("foobar")

	atok := client.AuthToken("authorized")
	authMgr := &testAuthManager{
		authorized: true,
		authtoken:  atok,
	}

	srv := cltest.NewClientTestServer()
	defer srv.Close()

	mender := newTestMender(&runner,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				authMgr: authMgr,
			},
		})
	// we should start with no token
	assert.Equal(t, atok, mender.authToken)

	// 1. client already authorized
	err := mender.Authorize()
	assert.NoError(t, err)
	// no need to build send request if auth data is valid
	assert.False(t, srv.Auth.Called)
	assert.Equal(t, atok, mender.authToken)

	// 2. pretend caching of authorization code fails
	authMgr.authtokenErr = errors.New("auth code load failed")
	mender.authToken = noAuthToken
	err = mender.Authorize()
	assert.Error(t, err)
	// no need to build send request if auth data is valid
	assert.False(t, srv.Auth.Called)
	assert.Equal(t, noAuthToken, mender.authToken)
	authMgr.authtokenErr = nil

	// 3. call the server, server denies authorization
	authMgr.authorized = false
	err = mender.Authorize()
	assert.Error(t, err)
	assert.False(t, err.IsFatal())
	assert.True(t, srv.Auth.Called)
	assert.Equal(t, noAuthToken, mender.authToken)

	// 4. pretend authorization manager fails to parse response
	srv.Auth.Called = false
	authMgr.testAuthDataMessenger.rspError = errors.New("response parse error")
	// we need the server authorize the client
	srv.Auth.Authorize = true
	srv.Auth.Token = rspdata
	err = mender.Authorize()
	assert.Error(t, err)
	assert.False(t, err.IsFatal())
	assert.True(t, srv.Auth.Called)
	// response data should be passed verbatim to AuthDataMessenger interface
	assert.Equal(t, rspdata, authMgr.testAuthDataMessenger.rspData)

	// 5. authorization manger throws no errors, server authorizes the client
	srv.Auth.Called = false
	authMgr.testAuthDataMessenger.rspError = nil
	// server will authorize the client
	srv.Auth.Authorize = true
	srv.Auth.Token = rspdata
	err = mender.Authorize()
	// all good
	assert.NoError(t, err)
	// Authorize() should have reloaded the cache (token comes from mock
	// auth manager)
	assert.Equal(t, atok, mender.authToken)
}

func TestMenderReportStatus(t *testing.T) {
	srv := cltest.NewClientTestServer()
	defer srv.Close()

	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)

	ms.WriteAll(authTokenName, []byte("tokendata"))

	err := mender.Authorize()
	assert.NoError(t, err)

	srv.Auth.Verify = true
	srv.Auth.Token = []byte("tokendata")

	// 1. successful report
	err = mender.ReportUpdateStatus(
		client.UpdateResponse{
			ID: "foobar",
		},
		client.StatusSuccess,
	)
	assert.Nil(t, err)
	assert.Equal(t, client.StatusSuccess, srv.Status.Status)

	// 2. pretend authorization fails, server expects a different token
	srv.Reset()
	srv.Auth.Token = []byte("footoken")
	srv.Auth.Verify = true
	err = mender.ReportUpdateStatus(
		client.UpdateResponse{
			ID: "foobar",
		},
		client.StatusSuccess,
	)
	assert.NotNil(t, err)
	assert.False(t, err.IsFatal())

	// 3. pretend that deployment was aborted
	srv.Reset()
	srv.Auth.Token = []byte("tokendata")
	srv.Auth.Verify = true
	srv.Status.Aborted = true
	err = mender.ReportUpdateStatus(
		client.UpdateResponse{
			ID: "foobar",
		},
		client.StatusSuccess,
	)
	assert.NotNil(t, err)
	assert.True(t, err.IsFatal())
}

func TestMenderLogUpload(t *testing.T) {
	srv := cltest.NewClientTestServer()
	defer srv.Close()

	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)

	ms.WriteAll(authTokenName, []byte("tokendata"))

	err := mender.Authorize()
	assert.NoError(t, err)

	srv.Auth.Verify = true
	srv.Auth.Token = []byte("tokendata")

	// 1. log upload successful
	logs := []byte(`{ "messages":
[{ "time": "12:12:12", "level": "error", "msg": "log foo" },
{ "time": "12:12:13", "level": "debug", "msg": "log bar" }]
}`)

	err = mender.UploadLog(
		client.UpdateResponse{
			ID: "foobar",
		},
		logs,
	)
	assert.Nil(t, err)
	assert.JSONEq(t, `{
	  "messages": [
	      {
	          "time": "12:12:12",
	          "level": "error",
	          "msg": "log foo"
	      },
	      {
	          "time": "12:12:13",
	          "level": "debug",
	          "msg": "log bar"
	      }
	   ]}`, string(srv.Log.Logs))

	// 2. pretend authorization fails, server expects a different token
	srv.Auth.Token = []byte("footoken")
	err = mender.UploadLog(
		client.UpdateResponse{
			ID: "foobar",
		},
		logs,
	)
	assert.NotNil(t, err)
}

func TestMenderState(t *testing.T) {
	d, err := json.Marshal(MenderStateInit)

	assert.Equal(t, []byte(`"init"`), d)
	assert.NoError(t, err)

	d, err = json.Marshal(MenderState(333))
	assert.Error(t, err)
	assert.Empty(t, d)

	var s MenderState
	err = json.Unmarshal([]byte(`"init"`), &s)

	assert.NoError(t, err)
	assert.Equal(t, MenderStateInit, s)
}

func TestAuthToken(t *testing.T) {
	ts := cltest.NewClientTestServer()
	defer ts.Close()

	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{
			ServerURL: ts.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)
	ms.WriteAll(authTokenName, []byte("tokendata"))
	token, err := ms.ReadAll(authTokenName)
	assert.NoError(t, err)
	assert.Equal(t, []byte("tokendata"), token)

	ts.Update.Unauthorized = true

	_, updErr := mender.CheckUpdate()
	assert.EqualError(t, updErr.Cause(), client.ErrNotAuthorized.Error())

	token, err = ms.ReadAll(authTokenName)
	assert.Equal(t, os.ErrNotExist, err)
	assert.Empty(t, token)
}

func TestMenderInventoryRefresh(t *testing.T) {
	// create temp dir
	td, _ := ioutil.TempDir("", "mender-install-update-")
	defer os.RemoveAll(td)

	// prepare fake artifactInfo file, it is read when submitting inventory to
	// fill some default fields (device_type, artifact_name)
	artifactInfo := path.Join(td, "artifact_info")
	ioutil.WriteFile(artifactInfo, []byte("artifact_name=fake-id"), 0600)
	deviceType := path.Join(td, "device_type")
	ioutil.WriteFile(deviceType, []byte("device_type=foo-bar"), 0600)

	srv := cltest.NewClientTestServer()
	defer srv.Close()

	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		},
	)
	mender.artifactInfoFile = artifactInfo
	mender.deviceTypeFile = deviceType

	ms.WriteAll(authTokenName, []byte("tokendata"))

	merr := mender.Authorize()
	assert.NoError(t, merr)

	// prepare fake inventory scripts
	// 1. setup a temporary path $TMPDIR/mendertest<random>/inventory
	tdir, err := ioutil.TempDir("", "mendertest")
	assert.NoError(t, err)
	invpath := path.Join(tdir, "inventory")
	err = os.MkdirAll(invpath, os.FileMode(syscall.S_IRWXU))
	assert.NoError(t, err)
	defer os.RemoveAll(tdir)

	oldDefaultPathDataDir := defaultPathDataDir
	// override datadir path for subsequent getDataDirPath() calls
	defaultPathDataDir = tdir

	// 1a. no scripts hence no inventory data, submit should have been
	// called with default inventory attributes only
	srv.Auth.Verify = true
	srv.Auth.Token = []byte("tokendata")
	err = mender.InventoryRefresh()
	assert.Nil(t, err)

	assert.True(t, srv.Inventory.Called)
	exp := []client.InventoryAttribute{
		{Name: "device_type", Value: "foo-bar"},
		{Name: "artifact_name", Value: "fake-id"},
		{Name: "mender_client_version", Value: "unknown"},
	}
	for _, a := range exp {
		assert.Contains(t, srv.Inventory.Attrs, a)
	}

	// 2. fake inventory script
	err = ioutil.WriteFile(path.Join(invpath, "mender-inventory-foo"),
		[]byte(`#!/bin/sh
echo foo=bar`),
		os.FileMode(syscall.S_IRWXU))
	assert.NoError(t, err)

	srv.Reset()
	srv.Auth.Verify = true
	srv.Auth.Token = []byte("tokendata")
	err = mender.InventoryRefresh()
	assert.Nil(t, err)
	exp = []client.InventoryAttribute{
		{Name: "device_type", Value: "foo-bar"},
		{Name: "artifact_name", Value: "fake-id"},
		{Name: "mender_client_version", Value: "unknown"},
		{Name: "foo", Value: "bar"},
	}
	for _, a := range exp {
		assert.Contains(t, srv.Inventory.Attrs, a)
	}

	// 3. pretend client is no longer authorized
	srv.Auth.Token = []byte("footoken")
	err = mender.InventoryRefresh()
	assert.NotNil(t, err)

	// restore old datadir path
	defaultPathDataDir = oldDefaultPathDataDir
}

func MakeFakeUpdate(data string) (string, error) {
	f, err := ioutil.TempFile("", "test_update")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if len(data) > 0 {
		if _, err := f.WriteString(data); err != nil {
			return "", err
		}
	}
	return f.Name(), nil
}

type rc struct {
	*bytes.Buffer
}

func (r *rc) Close() error {
	return nil
}

const (
	PublicRSAKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDSTLzZ9hQq3yBB+dMDVbKem6ia
v1J6opg6DICKkQ4M/yhlw32BCGm2ArM3VwQRgq6Q1sNSq953n5c1EO3Xcy/qTAKc
XwaUNml5EhW79AdibBXZiZt8fMhCjUd/4ce3rLNjnbIn1o9L6pzV4CcVJ8+iNhne
5vbA+63vRCnrc8QuYwIDAQAB
-----END PUBLIC KEY-----`
	PrivateRSAKey = `-----BEGIN RSA PRIVATE KEY-----
MIICXAIBAAKBgQDSTLzZ9hQq3yBB+dMDVbKem6iav1J6opg6DICKkQ4M/yhlw32B
CGm2ArM3VwQRgq6Q1sNSq953n5c1EO3Xcy/qTAKcXwaUNml5EhW79AdibBXZiZt8
fMhCjUd/4ce3rLNjnbIn1o9L6pzV4CcVJ8+iNhne5vbA+63vRCnrc8QuYwIDAQAB
AoGAQKIRELQOsrZsxZowfj/ia9jPUvAmO0apnn2lK/E07k2lbtFMS1H4m1XtGr8F
oxQU7rLyyP/FmeJUqJyRXLwsJzma13OpxkQtZmRpL9jEwevnunHYJfceVapQOJ7/
6Oz0pPWEq39GCn+tTMtgSmkEaSH8Ki9t32g9KuQIKBB2hbECQQDsg7D5fHQB1BXG
HJm9JmYYX0Yk6Z2SWBr4mLO0C4hHBnV5qPCLyevInmaCV2cOjDZ5Sz6iF5RK5mw7
qzvFa8ePAkEA46Anom3cNXO5pjfDmn2CoqUvMeyrJUFL5aU6W1S6iFprZ/YwdHcC
kS5yTngwVOmcnT65Vnycygn+tZan2A0h7QJBAJNlowZovDdjgEpeCqXp51irD6Dz
gsLwa6agK+Y6Ba0V5mJyma7UoT//D62NYOmdElnXPepwvXdMUQmCtpZbjBsCQD5H
VHDJlCV/yzyiJz9+tZ5giaAkO9NOoUBsy6GvdfXWn2prXmiPI0GrrpSvp7Gj1Tjk
r3rtT0ysHWd7l+Kx/SUCQGlitd5RDfdHl+gKrCwhNnRG7FzRLv5YOQV81+kh7SkU
73TXPIqLESVrqWKDfLwfsfEpV248MSRou+y0O1mtFpo=
-----END RSA PRIVATE KEY-----`
)

func MakeRootfsImageArtifact(version int, signed bool) (io.ReadCloser, error) {
	upd, err := MakeFakeUpdate("test update")
	if err != nil {
		return nil, err
	}
	defer os.Remove(upd)

	art := bytes.NewBuffer(nil)
	var aw *awriter.Writer
	if !signed {
		aw = awriter.NewWriter(art)
	} else {
		s := artifact.NewSigner([]byte(PrivateRSAKey))
		aw = awriter.NewWriterSigned(art, s)
	}
	var u handlers.Composer
	switch version {
	case 1:
		u = handlers.NewRootfsV1(upd)
	case 2:
		u = handlers.NewRootfsV2(upd)
	}

	updates := &awriter.Updates{U: []handlers.Composer{u}}
	err = aw.WriteArtifact("mender", version, []string{"vexpress-qemu"},
		"mender-1.1", updates, nil)
	if err != nil {
		return nil, err
	}
	return &rc{art}, nil
}

type mockReader struct {
	mock.Mock
}

func (m *mockReader) Read(p []byte) (int, error) {
	ret := m.Called()
	return ret.Get(0).(int), ret.Error(1)
}

func TestMenderInstallUpdate(t *testing.T) {
	// create temp dir
	td, _ := ioutil.TempDir("", "mender-install-update-")
	defer os.RemoveAll(td)

	// prepare fake artifactInfo file, with bogus
	deviceType := path.Join(td, "device_type")

	mender := newTestMender(nil, menderConfig{},
		testMenderPieces{
			MenderPieces: MenderPieces{
				device: &fakeDevice{consumeUpdate: true},
			},
		},
	)
	mender.deviceTypeFile = deviceType

	// try some failure scenarios first

	// EOF
	err := mender.InstallUpdate(ioutil.NopCloser(&bytes.Buffer{}), 0)
	assert.Error(t, err)
	t.Logf("error: %v", err)

	// some error from reader
	mr := mockReader{}
	mr.On("Read").Return(0, errors.New("failed"))
	err = mender.InstallUpdate(ioutil.NopCloser(&mr), 0)
	assert.Error(t, err)
	t.Logf("error: %v", err)

	// make a fake update artifact
	upd, err := MakeRootfsImageArtifact(1, false)
	assert.NoError(t, err)
	assert.NotNil(t, upd)

	// setup soem bogus device_type so that we don't match the update
	ioutil.WriteFile(deviceType, []byte("device_type=bogusdevicetype\n"), 0644)
	err = mender.InstallUpdate(upd, 0)
	assert.Error(t, err)

	// try with a legit device_type
	upd, err = MakeRootfsImageArtifact(1, false)
	assert.NoError(t, err)
	assert.NotNil(t, upd)

	ioutil.WriteFile(deviceType, []byte("device_type=vexpress-qemu\n"), 0644)
	err = mender.InstallUpdate(upd, 0)
	assert.NoError(t, err)

	// now try with device throwing errors durin ginstall
	upd, err = MakeRootfsImageArtifact(1, false)
	assert.NoError(t, err)
	assert.NotNil(t, upd)

	mender = newTestMender(nil, menderConfig{},
		testMenderPieces{
			MenderPieces: MenderPieces{
				device: &fakeDevice{retInstallUpdate: errors.New("failed")},
			},
		},
	)
	mender.deviceTypeFile = deviceType
	err = mender.InstallUpdate(upd, 0)
	assert.Error(t, err)

}

func TestMenderFetchUpdate(t *testing.T) {
	srv := cltest.NewClientTestServer()
	defer srv.Close()

	srv.Update.Has = true

	ms := store.NewMemStore()
	mender := newTestMender(nil,
		menderConfig{
			ServerURL: srv.URL,
		},
		testMenderPieces{
			MenderPieces: MenderPieces{
				store: ms,
			},
		})

	ms.WriteAll(authTokenName, []byte("tokendata"))
	merr := mender.Authorize()
	assert.NoError(t, merr)

	// populate download data with random bytes
	rdata := bytes.Buffer{}
	rcount := 8192
	_, err := io.CopyN(&rdata, rand.Reader, int64(rcount))
	assert.NoError(t, err)
	assert.Equal(t, rcount, rdata.Len())
	rbytes := rdata.Bytes()

	_, err = io.Copy(&srv.UpdateDownload.Data, &rdata)
	assert.NoError(t, err)
	assert.Equal(t, rcount, len(rbytes))

	img, sz, err := mender.FetchUpdate(srv.URL + "/api/devices/v1/download")
	assert.NoError(t, err)
	assert.NotNil(t, img)
	assert.EqualValues(t, len(rbytes), sz)

	dl := bytes.Buffer{}
	_, err = io.Copy(&dl, img)
	assert.NoError(t, err)
	assert.EqualValues(t, sz, dl.Len())

	assert.True(t, bytes.Equal(rbytes, dl.Bytes()))
}
