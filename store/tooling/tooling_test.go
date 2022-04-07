// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2022 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package tooling_test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/bootloadertest"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/progress"
	"github.com/snapcore/snapd/seed/seedtest"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/store"
	"github.com/snapcore/snapd/store/tooling"
	"github.com/snapcore/snapd/testutil"
)

func Test(t *testing.T) { TestingT(t) }

type toolingSuite struct {
	testutil.BaseTest
	root       string
	bootloader *bootloadertest.MockBootloader

	stdout *bytes.Buffer
	stderr *bytes.Buffer

	storeActionsBunchSizes []int
	storeActions           []*store.SnapAction
	curSnaps               [][]*store.CurrentSnap

	tsto *tooling.ToolingStore

	// SeedSnaps helps creating and making available seed snaps
	// (it provides MakeAssertedSnap etc.) for the tests.
	*seedtest.SeedSnaps

	model *asserts.Model
}

var _ = Suite(&toolingSuite{})

var (
	brandPrivKey, _ = assertstest.GenerateKey(752)
)

// TODO: use seedtest.SampleSnapYaml for some of these
const packageGadget = `
name: pc
version: 1.0
type: gadget
`

const packageGadgetWithBase = `
name: pc18
version: 1.0
type: gadget
base: core18
`
const packageClassicGadget = `
name: classic-gadget
version: 1.0
type: gadget
`

const packageClassicGadget18 = `
name: classic-gadget18
version: 1.0
type: gadget
base: core18
`

const packageKernel = `
name: pc-kernel
version: 4.4-1
type: kernel
`

const packageCore = `
name: core
version: 16.04
type: os
`

const packageCore18 = `
name: core18
version: 18.04
type: base
`

const snapdSnap = `
name: snapd
version: 3.14
type: snapd
`

const otherBase = `
name: other-base
version: 2.5029
type: base
`

const requiredSnap1 = `
name: required-snap1
version: 1.0
`

const requiredSnap18 = `
name: required-snap18
version: 1.0
base: core18
`

const defaultTrackSnap18 = `
name: default-track-snap18
version: 1.0
base: core18
`

const snapReqOtherBase = `
name: snap-req-other-base
version: 1.0
base: other-base
`

const snapReqCore16Base = `
name: snap-req-core16-base
version: 1.0
base: core16
`

const snapReqContentProvider = `
name: snap-req-content-provider
version: 1.0
plugs:
 gtk-3-themes:
  interface: content
  default-provider: gtk-common-themes
  target: $SNAP/data-dir/themes
`

const snapBaseNone = `
name: snap-base-none
version: 1.0
base: none
`

func (s *toolingSuite) SetUpTest(c *C) {
	s.root = c.MkDir()
	s.bootloader = bootloadertest.Mock("grub", c.MkDir())
	bootloader.Force(s.bootloader)

	s.BaseTest.SetUpTest(c)
	s.BaseTest.AddCleanup(snap.MockSanitizePlugsSlots(func(snapInfo *snap.Info) {}))

	s.tsto = tooling.MockToolingStore(s)

	s.SeedSnaps = &seedtest.SeedSnaps{}
	s.SetupAssertSigning("canonical")
	s.Brands.Register("my-brand", brandPrivKey, map[string]interface{}{
		"verification": "verified",
	})
	assertstest.AddMany(s.StoreSigning, s.Brands.AccountsAndKeys("my-brand")...)

	s.model = s.Brands.Model("my-brand", "my-model", map[string]interface{}{
		"display-name":   "my display name",
		"architecture":   "amd64",
		"gadget":         "pc",
		"kernel":         "pc-kernel",
		"required-snaps": []interface{}{"required-snap1"},
	})

	otherAcct := assertstest.NewAccount(s.StoreSigning, "other", map[string]interface{}{
		"account-id": "other",
	}, "")
	s.StoreSigning.Add(otherAcct)

	// mock the mount cmds (for the extract kernel assets stuff)
	c1 := testutil.MockCommand(c, "mount", "")
	s.AddCleanup(c1.Restore)
	c2 := testutil.MockCommand(c, "umount", "")
	s.AddCleanup(c2.Restore)
}

func (s *toolingSuite) MakeAssertedSnap(c *C, snapYaml string, files [][]string, revision snap.Revision, developerID string) {
	s.SeedSnaps.MakeAssertedSnap(c, snapYaml, files, revision, developerID, s.StoreSigning.Database)
}

const stableChannel = "stable"

const pcGadgetYaml = `
 volumes:
   pc:
     bootloader: grub
 `

func (s *toolingSuite) setupSnaps(c *C, publishers map[string]string, defaultsYaml string) {
	gadgetYaml := pcGadgetYaml + defaultsYaml
	if _, ok := publishers["pc"]; ok {
		s.MakeAssertedSnap(c, packageGadget, [][]string{
			{"grub.conf", ""}, {"grub.cfg", "I'm a grub.cfg"},
			{"meta/gadget.yaml", gadgetYaml},
		}, snap.R(1), publishers["pc"])
	}
	if _, ok := publishers["pc18"]; ok {
		s.MakeAssertedSnap(c, packageGadgetWithBase, [][]string{
			{"grub.conf", ""}, {"grub.cfg", "I'm a grub.cfg"},
			{"meta/gadget.yaml", gadgetYaml},
		}, snap.R(4), publishers["pc18"])
	}

	if _, ok := publishers["classic-gadget"]; ok {
		s.MakeAssertedSnap(c, packageClassicGadget, [][]string{
			{"some-file", "Some file"},
		}, snap.R(5), publishers["classic-gadget"])
	}

	if _, ok := publishers["classic-gadget18"]; ok {
		s.MakeAssertedSnap(c, packageClassicGadget18, [][]string{
			{"some-file", "Some file"},
		}, snap.R(5), publishers["classic-gadget18"])
	}

	if _, ok := publishers["pc-kernel"]; ok {
		s.MakeAssertedSnap(c, packageKernel, nil, snap.R(2), publishers["pc-kernel"])
	}

	s.MakeAssertedSnap(c, packageCore, nil, snap.R(3), "canonical")

	s.MakeAssertedSnap(c, packageCore18, nil, snap.R(18), "canonical")
	s.MakeAssertedSnap(c, snapdSnap, nil, snap.R(18), "canonical")

	s.MakeAssertedSnap(c, otherBase, nil, snap.R(18), "other")

	s.MakeAssertedSnap(c, snapReqCore16Base, nil, snap.R(16), "other")

	s.MakeAssertedSnap(c, requiredSnap1, nil, snap.R(3), "other")
	s.AssertedSnapInfo("required-snap1").EditedContact = "mailto:foo@example.com"

	s.MakeAssertedSnap(c, requiredSnap18, nil, snap.R(6), "other")
	s.AssertedSnapInfo("required-snap18").EditedContact = "mailto:foo@example.com"

	s.MakeAssertedSnap(c, defaultTrackSnap18, nil, snap.R(5), "other")

	s.MakeAssertedSnap(c, snapReqOtherBase, nil, snap.R(5), "other")

	s.MakeAssertedSnap(c, snapReqContentProvider, nil, snap.R(5), "other")

	s.MakeAssertedSnap(c, snapBaseNone, nil, snap.R(1), "other")
}

func (s *toolingSuite) TestNewToolingStoreWithAuth(c *C) {
	tmpdir := c.MkDir()
	authFn := filepath.Join(tmpdir, "auth.json")
	err := ioutil.WriteFile(authFn, []byte(`{
"macaroon": "MACAROON",
"discharges": ["DISCHARGE"]
}`), 0600)
	c.Assert(err, IsNil)

	os.Setenv("UBUNTU_STORE_AUTH_DATA_FILENAME", authFn)
	defer os.Unsetenv("UBUNTU_STORE_AUTH_DATA_FILENAME")

	tsto, err := tooling.NewToolingStore()
	c.Assert(err, IsNil)
	user := tsto.User()
	c.Check(user.StoreMacaroon, Equals, "MACAROON")
	c.Check(user.StoreDischarges, DeepEquals, []string{"DISCHARGE"})
}

func (s *toolingSuite) TestNewToolingStoreWithAuthFromSnapcraftLoginFile(c *C) {
	tmpdir := c.MkDir()
	authFn := filepath.Join(tmpdir, "auth.json")
	err := ioutil.WriteFile(authFn, []byte(`[login.ubuntu.com]
macaroon = MACAROON
unbound_discharge = DISCHARGE

`), 0600)
	c.Assert(err, IsNil)

	os.Setenv("UBUNTU_STORE_AUTH_DATA_FILENAME", authFn)
	defer os.Unsetenv("UBUNTU_STORE_AUTH_DATA_FILENAME")

	tsto, err := tooling.NewToolingStore()
	c.Assert(err, IsNil)
	user := tsto.User()
	c.Check(user.StoreMacaroon, Equals, "MACAROON")
	c.Check(user.StoreDischarges, DeepEquals, []string{"DISCHARGE"})
}

func (s *toolingSuite) TestDownloadpOptionsString(c *C) {
	tests := []struct {
		opts tooling.DownloadSnapOptions
		str  string
	}{
		{tooling.DownloadSnapOptions{LeavePartialOnError: true}, ""},
		{tooling.DownloadSnapOptions{}, ""},
		{tooling.DownloadSnapOptions{TargetDir: "/foo"}, `in "/foo"`},
		{tooling.DownloadSnapOptions{Basename: "foo"}, `to "foo.snap"`},
		{tooling.DownloadSnapOptions{Channel: "foo"}, `from channel "foo"`},
		{tooling.DownloadSnapOptions{Revision: snap.R(42)}, `(42)`},
		{tooling.DownloadSnapOptions{
			CohortKey: "AbCdEfGhIjKlMnOpQrStUvWxYz",
		}, `from cohort "…rStUvWxYz"`},
		{tooling.DownloadSnapOptions{
			TargetDir: "/foo",
			Basename:  "bar",
			Channel:   "baz",
			Revision:  snap.R(13),
			CohortKey: "MSBIc3dwOW9PemozYjRtdzhnY0MwMFh0eFduS0g5UWlDUSAxNTU1NDExNDE1IDBjYzJhNTc1ZjNjOTQ3ZDEwMWE1NTNjZWFkNmFmZDE3ZWJhYTYyNjM4ZWQ3ZGMzNjI5YmU4YjQ3NzAwMjdlMDk=",
		}, `(13) from channel "baz" from cohort "…wMjdlMDk=" to "bar.snap" in "/foo"`}, // note this one is not 'valid' so it's ok if the string is a bit wonky

	}

	for _, t := range tests {
		c.Check(t.opts.String(), Equals, t.str)
	}
}

func (s *toolingSuite) TestDownloadSnapOptionsValid(c *C) {
	tests := []struct {
		opts tooling.DownloadSnapOptions
		err  error
	}{
		{tooling.DownloadSnapOptions{}, nil}, // might want to error if no targetdir
		{tooling.DownloadSnapOptions{TargetDir: "foo"}, nil},
		{tooling.DownloadSnapOptions{Channel: "foo"}, nil},
		{tooling.DownloadSnapOptions{Revision: snap.R(42)}, nil},
		{tooling.DownloadSnapOptions{
			CohortKey: "AbCdEfGhIjKlMnOpQrStUvWxYz",
		}, nil},
		{tooling.DownloadSnapOptions{
			Channel:  "foo",
			Revision: snap.R(42),
		}, nil},
		{tooling.DownloadSnapOptions{
			Channel:   "foo",
			CohortKey: "bar",
		}, nil},
		{tooling.DownloadSnapOptions{
			Revision:  snap.R(1),
			CohortKey: "bar",
		}, tooling.ErrRevisionAndCohort},
		{tooling.DownloadSnapOptions{
			Basename: "/foo",
		}, tooling.ErrPathInBase},
	}

	for _, t := range tests {
		t.opts.LeavePartialOnError = true
		c.Check(t.opts.Validate(), Equals, t.err)
		t.opts.LeavePartialOnError = false
		c.Check(t.opts.Validate(), Equals, t.err)
	}
}

func (s *toolingSuite) TestDownloadSnap(c *C) {
	// TODO: maybe expand on this (test coverage of DownloadSnap is really bad)

	// env shenanigans
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	debug, hadDebug := os.LookupEnv("SNAPD_DEBUG")
	os.Setenv("SNAPD_DEBUG", "1")
	if hadDebug {
		defer os.Setenv("SNAPD_DEBUG", debug)
	} else {
		defer os.Unsetenv("SNAPD_DEBUG")
	}
	logbuf, restore := logger.MockLogger()
	defer restore()

	s.setupSnaps(c, map[string]string{
		"core": "canonical",
	}, "")

	dlDir := c.MkDir()
	opts := tooling.DownloadSnapOptions{
		TargetDir: dlDir,
	}
	dlSnap, err := s.tsto.DownloadSnap("core", opts)
	c.Assert(err, IsNil)
	c.Check(dlSnap.Path, Matches, filepath.Join(dlDir, `core_\d+.snap`))
	c.Check(dlSnap.Info.SnapName(), Equals, "core")
	c.Check(dlSnap.RedirectChannel, Equals, "")

	c.Check(logbuf.String(), Matches, `.* DEBUG: Going to download snap "core" `+opts.String()+".\n")
}

// interface for the store
func (s *toolingSuite) SnapAction(_ context.Context, curSnaps []*store.CurrentSnap, actions []*store.SnapAction, assertQuery store.AssertionQuery, _ *auth.UserState, _ *store.RefreshOptions) ([]store.SnapActionResult, []store.AssertionResult, error) {
	if assertQuery != nil {
		return nil, nil, fmt.Errorf("unexpected assertion query")
	}

	s.storeActionsBunchSizes = append(s.storeActionsBunchSizes, len(actions))
	s.curSnaps = append(s.curSnaps, curSnaps)
	sars := make([]store.SnapActionResult, 0, len(actions))
	for _, a := range actions {
		if a.Action != "download" {
			return nil, nil, fmt.Errorf("unexpected action %q", a.Action)
		}

		if _, instanceKey := snap.SplitInstanceName(a.InstanceName); instanceKey != "" {
			return nil, nil, fmt.Errorf("unexpected instance key in %q", a.InstanceName)
		}
		// record
		s.storeActions = append(s.storeActions, a)

		info := s.AssertedSnapInfo(a.InstanceName)
		if info == nil {
			return nil, nil, fmt.Errorf("no %q in the fake store", a.InstanceName)
		}
		info1 := *info
		channel := a.Channel
		redirectChannel := ""
		if strings.HasPrefix(a.InstanceName, "default-track-") {
			channel = "default-track/stable"
			redirectChannel = channel
		}
		info1.Channel = channel
		sars = append(sars, store.SnapActionResult{
			Info:            &info1,
			RedirectChannel: redirectChannel,
		})
	}

	return sars, nil, nil
}

func (s *toolingSuite) Download(ctx context.Context, name, targetFn string, downloadInfo *snap.DownloadInfo, pbar progress.Meter, user *auth.UserState, dlOpts *store.DownloadOptions) error {
	return osutil.CopyFile(s.AssertedSnap(name), targetFn, 0)
}

func (s *toolingSuite) Assertion(assertType *asserts.AssertionType, primaryKey []string, user *auth.UserState) (asserts.Assertion, error) {
	ref := &asserts.Ref{Type: assertType, PrimaryKey: primaryKey}
	return ref.Resolve(s.StoreSigning.Find)
}

type toolingStoreContextSuite struct {
	sc store.DeviceAndAuthContext
}

var _ = Suite(&toolingStoreContextSuite{})

func (s *toolingStoreContextSuite) SetUpTest(c *C) {
	s.sc = tooling.ToolingStoreContext()
}

func (s *toolingStoreContextSuite) TestNopBits(c *C) {
	info, err := s.sc.CloudInfo()
	c.Assert(err, IsNil)
	c.Check(info, IsNil)

	device, err := s.sc.Device()
	c.Assert(err, IsNil)
	c.Check(device, DeepEquals, &auth.DeviceState{})

	p, err := s.sc.DeviceSessionRequestParams("")
	c.Assert(err, Equals, store.ErrNoSerial)
	c.Check(p, IsNil)

	defURL, err := url.Parse("http://store")
	c.Assert(err, IsNil)
	proxyStoreID, proxyStoreURL, err := s.sc.ProxyStoreParams(defURL)
	c.Assert(err, IsNil)
	c.Check(proxyStoreID, Equals, "")
	c.Check(proxyStoreURL, Equals, defURL)

	storeID, err := s.sc.StoreID("")
	c.Assert(err, IsNil)
	c.Check(storeID, Equals, "")

	storeID, err = s.sc.StoreID("my-store")
	c.Assert(err, IsNil)
	c.Check(storeID, Equals, "my-store")

	_, err = s.sc.UpdateDeviceAuth(nil, "")
	c.Assert(err, NotNil)
}

func (s *toolingStoreContextSuite) TestUpdateUserAuth(c *C) {
	u := &auth.UserState{
		StoreMacaroon:   "macaroon",
		StoreDischarges: []string{"discharge1"},
	}

	u1, err := s.sc.UpdateUserAuth(u, []string{"discharge2"})
	c.Assert(err, IsNil)
	c.Check(u1, Equals, u)
	c.Check(u1.StoreDischarges, DeepEquals, []string{"discharge2"})
}
