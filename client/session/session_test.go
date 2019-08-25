/*
 Copyright 2013-2014 Canonical Ltd.

 This program is free software: you can redistribute it and/or modify it
 under the terms of the GNU General Public License version 3, as published
 by the Free Software Foundation.

 This program is distributed in the hope that it will be useful, but
 WITHOUT ANY WARRANTY; without even the implied warranties of
 MERCHANTABILITY, SATISFACTORY QUALITY, or FITNESS FOR A PARTICULAR
 PURPOSE.  See the GNU General Public License for more details.

 You should have received a copy of the GNU General Public License along
 with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package session

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	. "launchpad.net/gocheck"

	"github.com/ubports/ubuntu-push/click"
	"github.com/ubports/ubuntu-push/client/gethosts"
	"github.com/ubports/ubuntu-push/client/session/seenstate"
	"github.com/ubports/ubuntu-push/protocol"
	helpers "github.com/ubports/ubuntu-push/testing"
	"github.com/ubports/ubuntu-push/testing/condition"
	"github.com/ubports/ubuntu-push/util"
)

func TestSession(t *testing.T) { TestingT(t) }

//
// helpers! candidates to live in their own ../testing/ package.
//

type xAddr string

func (x xAddr) Network() string { return "<:>" }
func (x xAddr) String() string  { return string(x) }

// testConn (roughly based on the one in protocol_test)

type testConn struct {
	Name              string
	Deadlines         []time.Duration
	Writes            [][]byte
	WriteCondition    condition.Interface
	DeadlineCondition condition.Interface
	CloseCondition    condition.Interface
}

func (tc *testConn) LocalAddr() net.Addr { return xAddr(tc.Name) }

func (tc *testConn) RemoteAddr() net.Addr { return xAddr(tc.Name) }

func (tc *testConn) Close() error {
	if tc.CloseCondition == nil || tc.CloseCondition.OK() {
		return nil
	} else {
		return errors.New("closer on fire")
	}
}

func (tc *testConn) SetDeadline(t time.Time) error {
	tc.Deadlines = append(tc.Deadlines, t.Sub(time.Now()))
	if tc.DeadlineCondition == nil || tc.DeadlineCondition.OK() {
		return nil
	} else {
		return errors.New("deadliner on fire")
	}
}

func (tc *testConn) SetReadDeadline(t time.Time) error  { panic("SetReadDeadline not implemented.") }
func (tc *testConn) SetWriteDeadline(t time.Time) error { panic("SetWriteDeadline not implemented.") }
func (tc *testConn) Read(buf []byte) (n int, err error) { panic("Read not implemented.") }

func (tc *testConn) Write(buf []byte) (int, error) {
	store := make([]byte, len(buf))
	copy(store, buf)
	tc.Writes = append(tc.Writes, store)
	if tc.WriteCondition == nil || tc.WriteCondition.OK() {
		return len(store), nil
	} else {
		return -1, errors.New("writer on fire")
	}
}

// test protocol (from session_test)

type testProtocol struct {
	up   chan interface{}
	down chan interface{}
}

// takeNext takes a value from given channel with a 5s timeout
func takeNext(ch <-chan interface{}) interface{} {
	select {
	case <-time.After(5 * time.Second):
		panic("test protocol exchange stuck: too long waiting")
	case v := <-ch:
		return v
	}
	return nil
}

func (c *testProtocol) SetDeadline(t time.Time) {
	deadAfter := t.Sub(time.Now())
	deadAfter = (deadAfter + time.Millisecond/2) / time.Millisecond * time.Millisecond
	c.down <- fmt.Sprintf("deadline %v", deadAfter)
}

func (c *testProtocol) ReadMessage(dest interface{}) error {
	switch v := takeNext(c.up).(type) {
	case error:
		return v
	default:
		// make sure JSON.Unmarshal works with dest
		var marshalledMsg []byte
		marshalledMsg, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("can't jsonify test value %v: %s", v, err)
		}
		return json.Unmarshal(marshalledMsg, dest)
	}
	return nil
}

func (c *testProtocol) WriteMessage(src interface{}) error {
	// make sure JSON.Marshal works with src
	_, err := json.Marshal(src)
	if err != nil {
		return err
	}
	val := reflect.ValueOf(src)
	if val.Kind() == reflect.Ptr {
		src = val.Elem().Interface()
	}
	c.down <- src
	switch v := takeNext(c.up).(type) {
	case error:
		return v
	}
	return nil
}

// brokenSeenState is a SeenState that always breaks
type brokenSeenState struct{}

func (*brokenSeenState) SetLevel(string, int64) error            { return errors.New("broken.") }
func (*brokenSeenState) GetAllLevels() (map[string]int64, error) { return nil, errors.New("broken.") }
func (*brokenSeenState) Close()                                  {}
func (*brokenSeenState) FilterBySeen([]protocol.Notification) ([]protocol.Notification, error) {
	return nil, errors.New("broken.")
}

/////

type clientSessionSuite struct {
	log  *helpers.TestLogger
	lvls func() (seenstate.SeenState, error)
}

func (cs *clientSessionSuite) SetUpTest(c *C) {
	cs.log = helpers.NewTestLogger(c, "debug")
}

// in-memory level map testing
var _ = Suite(&clientSessionSuite{lvls: seenstate.NewSeenState})

// sqlite level map testing
type clientSqlevelsSessionSuite struct{ clientSessionSuite }

var _ = Suite(&clientSqlevelsSessionSuite{})

func (cs *clientSqlevelsSessionSuite) SetUpSuite(c *C) {
	cs.lvls = func() (seenstate.SeenState, error) { return seenstate.NewSqliteSeenState(":memory:") }
}

func (cs *clientSessionSuite) TestStateString(c *C) {
	for _, i := range []struct {
		v ClientSessionState
		s string
	}{
		{Error, "Error"},
		{Pristine, "Pristine"},
		{Disconnected, "Disconnected"},
		{Connected, "Connected"},
		{Started, "Started"},
		{Running, "Running"},
		{Shutdown, "Shutdown"},
		{Unknown, fmt.Sprintf("??? (%d)", Unknown)},
	} {
		c.Check(i.v.String(), Equals, i.s)
	}
}

/****************************************************************
  parseServerAddrSpec() tests
****************************************************************/

func (cs *clientSessionSuite) TestParseServerAddrSpec(c *C) {
	hEp, fallbackHosts := parseServerAddrSpec("http://foo/hosts")
	c.Check(hEp, Equals, "http://foo/hosts")
	c.Check(fallbackHosts, IsNil)

	hEp, fallbackHosts = parseServerAddrSpec("foo:443")
	c.Check(hEp, Equals, "")
	c.Check(fallbackHosts, DeepEquals, []string{"foo:443"})

	hEp, fallbackHosts = parseServerAddrSpec("foo:443|bar:443")
	c.Check(hEp, Equals, "")
	c.Check(fallbackHosts, DeepEquals, []string{"foo:443", "bar:443"})
}

/****************************************************************
  NewSession() tests
****************************************************************/

func dummyConf() ClientSessionConfig {
	return ClientSessionConfig{
		BroadcastCh:     make(chan *BroadcastNotification, 5),
		NotificationsCh: make(chan AddressedNotification, 5),
	}
}

func (cs *clientSessionSuite) TestNewSessionPlainWorks(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Check(sess, NotNil)
	c.Check(err, IsNil)
	c.Check(sess.fallbackHosts, DeepEquals, []string{"foo:443"})
	// the session is happy and redial delayer is default
	c.Check(sess.ShouldDelay(), Equals, false)
	c.Check(fmt.Sprintf("%#v", sess.redialDelay), Equals, fmt.Sprintf("%#v", redialDelay))
	c.Check(sess.redialDelays, DeepEquals, util.Timeouts())
	// but no root CAs set
	c.Check(sess.TLS.RootCAs, IsNil)
	c.Check(sess.State(), Equals, Pristine)
	c.Check(sess.stopCh, NotNil)
	c.Check(sess.cmdCh, NotNil)
}

func (cs *clientSessionSuite) TestNewSessionHostEndpointWorks(c *C) {
	sess, err := NewSession("http://foo/hosts", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Check(sess.getHost, NotNil)
}

func (cs *clientSessionSuite) TestNewSessionPEMWorks(c *C) {
	pem, err := ioutil.ReadFile(helpers.SourceRelative("../../server/acceptance/ssl/testing.cert"))
	c.Assert(err, IsNil)
	conf := ClientSessionConfig{PEM: pem}
	sess, err := NewSession("", conf, "wah", cs.lvls, cs.log)
	c.Check(sess, NotNil)
	c.Assert(err, IsNil)
	c.Check(sess.TLS.RootCAs, NotNil)
}

func (cs *clientSessionSuite) TestNewSessionBadPEMFileContentFails(c *C) {
	badpem := []byte("This is not the PEM you're looking for.")
	conf := ClientSessionConfig{PEM: badpem}
	sess, err := NewSession("", conf, "wah", cs.lvls, cs.log)
	c.Check(sess, IsNil)
	c.Check(err, NotNil)
}

func (cs *clientSessionSuite) TestNewSessionBadSeenStateFails(c *C) {
	ferr := func() (seenstate.SeenState, error) { return nil, errors.New("Busted.") }
	sess, err := NewSession("", dummyConf(), "wah", ferr, cs.log)
	c.Check(sess, IsNil)
	c.Assert(err, NotNil)
}

/****************************************************************
  getHosts() tests
****************************************************************/

func (cs *clientSessionSuite) TestGetHostsFallback(c *C) {
	fallback := []string{"foo:443", "bar:443"}
	sess := &clientSession{fallbackHosts: fallback}
	err := sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, fallback)
}

type testHostGetter struct {
	domain string
	hosts  []string
	err    error
}

func (thg *testHostGetter) Get() (*gethosts.Host, error) {
	return &gethosts.Host{thg.domain, thg.hosts}, thg.err
}

func (cs *clientSessionSuite) TestGetHostsRemote(c *C) {
	hostGetter := &testHostGetter{"example.com", []string{"foo:443", "bar:443"}, nil}
	sess := &clientSession{getHost: hostGetter, timeSince: time.Since}
	err := sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, []string{"foo:443", "bar:443"})
}

func (cs *clientSessionSuite) TestGetHostsRemoteError(c *C) {
	sess, err := NewSession("", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	hostsErr := errors.New("failed")
	hostGetter := &testHostGetter{"", nil, hostsErr}
	sess.getHost = hostGetter
	err = sess.getHosts()
	c.Assert(err, Equals, hostsErr)
	c.Check(sess.deliveryHosts, IsNil)
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestGetHostsRemoteCaching(c *C) {
	hostGetter := &testHostGetter{"example.com", []string{"foo:443", "bar:443"}, nil}
	sess := &clientSession{
		getHost: hostGetter,
		ClientSessionConfig: ClientSessionConfig{
			HostsCachingExpiryTime: 2 * time.Hour,
		},
		timeSince: time.Since,
	}
	err := sess.getHosts()
	c.Assert(err, IsNil)
	hostGetter.hosts = []string{"baz:443"}
	// cached
	err = sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, []string{"foo:443", "bar:443"})
	// expired
	sess.timeSince = func(ts time.Time) time.Duration {
		return 3 * time.Hour
	}
	err = sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, []string{"baz:443"})
}

func (cs *clientSessionSuite) TestGetHostsRemoteCachingReset(c *C) {
	hostGetter := &testHostGetter{"example.com", []string{"foo:443", "bar:443"}, nil}
	sess := &clientSession{
		getHost: hostGetter,
		ClientSessionConfig: ClientSessionConfig{
			HostsCachingExpiryTime: 2 * time.Hour,
		},
		timeSince: time.Since,
	}
	err := sess.getHosts()
	c.Assert(err, IsNil)
	hostGetter.hosts = []string{"baz:443"}
	// cached
	err = sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, []string{"foo:443", "bar:443"})
	// reset
	sess.resetHosts()
	err = sess.getHosts()
	c.Assert(err, IsNil)
	c.Check(sess.deliveryHosts, DeepEquals, []string{"baz:443"})
}

/****************************************************************
  startConnectionAttempt()/nextHostToTry()/started tests
****************************************************************/

func (cs *clientSessionSuite) TestStartConnectionAttempt(c *C) {
	since := time.Since(time.Time{})
	sess := &clientSession{
		ClientSessionConfig: ClientSessionConfig{
			ExpectAllRepairedTime: 10 * time.Second,
		},
		timeSince: func(ts time.Time) time.Duration {
			return since
		},
		deliveryHosts: []string{"foo:443", "bar:443"},
	}
	// start from first host
	sess.startConnectionAttempt()
	c.Check(sess.lastAttemptTimestamp, Not(Equals), 0)
	c.Check(sess.tryHost, Equals, 0)
	c.Check(sess.leftToTry, Equals, 2)
	since = 1 * time.Second
	sess.tryHost = 1
	// just continue
	sess.startConnectionAttempt()
	c.Check(sess.tryHost, Equals, 1)
	sess.tryHost = 2
}

func (cs *clientSessionSuite) TestStartConnectionAttemptNoHostsPanic(c *C) {
	since := time.Since(time.Time{})
	sess := &clientSession{
		ClientSessionConfig: ClientSessionConfig{
			ExpectAllRepairedTime: 10 * time.Second,
		},
		timeSince: func(ts time.Time) time.Duration {
			return since
		},
	}
	c.Check(sess.startConnectionAttempt, PanicMatches, "should have got hosts from config or remote at this point")
}

func (cs *clientSessionSuite) TestNextHostToTry(c *C) {
	sess := &clientSession{
		deliveryHosts: []string{"foo:443", "bar:443", "baz:443"},
		tryHost:       0,
		leftToTry:     3,
	}
	c.Check(sess.nextHostToTry(), Equals, "foo:443")
	c.Check(sess.nextHostToTry(), Equals, "bar:443")
	c.Check(sess.nextHostToTry(), Equals, "baz:443")
	c.Check(sess.nextHostToTry(), Equals, "")
	c.Check(sess.nextHostToTry(), Equals, "")
	c.Check(sess.tryHost, Equals, 0)

	sess.leftToTry = 3
	sess.tryHost = 1
	c.Check(sess.nextHostToTry(), Equals, "bar:443")
	c.Check(sess.nextHostToTry(), Equals, "baz:443")
	c.Check(sess.nextHostToTry(), Equals, "foo:443")
	c.Check(sess.nextHostToTry(), Equals, "")
	c.Check(sess.nextHostToTry(), Equals, "")
	c.Check(sess.tryHost, Equals, 1)
}

func (cs *clientSessionSuite) TestStarted(c *C) {
	sess, err := NewSession("", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)

	sess.deliveryHosts = []string{"foo:443", "bar:443", "baz:443"}
	sess.tryHost = 1

	sess.started()
	c.Check(sess.tryHost, Equals, 0)
	c.Check(sess.State(), Equals, Started)

	sess.started()
	c.Check(sess.tryHost, Equals, 2)
}

/****************************************************************
  connect() tests
****************************************************************/

func (cs *clientSessionSuite) TestConnectFailsWithNoAddress(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.deliveryHosts = []string{"nowhere"}
	sess.clearShouldDelay()
	err = sess.connect()
	c.Check(sess.ShouldDelay(), Equals, true)
	c.Check(err, ErrorMatches, ".*connect.*address.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestConnectConnects(c *C) {
	srv, err := net.Listen("tcp", "localhost:0")
	c.Assert(err, IsNil)
	defer srv.Close()
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.deliveryHosts = []string{srv.Addr().String()}
	sess.clearShouldDelay()
	err = sess.connect()
	c.Check(sess.ShouldDelay(), Equals, true)
	c.Check(err, IsNil)
	c.Check(sess.Connection, NotNil)
	c.Check(sess.State(), Equals, Connected)
}

func (cs *clientSessionSuite) TestConnectSecondConnects(c *C) {
	srv, err := net.Listen("tcp", "localhost:0")
	c.Assert(err, IsNil)
	defer srv.Close()
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.deliveryHosts = []string{"nowhere", srv.Addr().String()}
	sess.clearShouldDelay()
	err = sess.connect()
	c.Check(sess.ShouldDelay(), Equals, true)
	c.Check(err, IsNil)
	c.Check(sess.Connection, NotNil)
	c.Check(sess.State(), Equals, Connected)
	c.Check(sess.tryHost, Equals, 0)
}

func (cs *clientSessionSuite) TestConnectConnectFail(c *C) {
	srv, err := net.Listen("tcp", "localhost:0")
	c.Assert(err, IsNil)
	sess, err := NewSession(srv.Addr().String(), dummyConf(), "wah", cs.lvls, cs.log)
	srv.Close()
	c.Assert(err, IsNil)
	sess.deliveryHosts = []string{srv.Addr().String()}
	sess.clearShouldDelay()
	err = sess.connect()
	c.Check(sess.ShouldDelay(), Equals, true)
	c.Check(err, ErrorMatches, ".*connection refused")
	c.Check(sess.State(), Equals, Error)
}

type dumbRetrier struct{ stopped bool }

func (*dumbRetrier) Redial() uint32 { return 0 }
func (d *dumbRetrier) Stop()        { d.stopped = true }

// /****************************************************************
//   AutoRedial() tests
// ****************************************************************/

func (cs *clientSessionSuite) TestAutoRedialWorks(c *C) {
	// checks that AutoRedial sets up a retrier and tries redialing it
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ar := new(dumbRetrier)
	sess.retrier = ar
	c.Check(ar.stopped, Equals, false)
	sess.autoRedial()
	defer sess.stopRedial()
	c.Check(ar.stopped, Equals, true)
}

func (cs *clientSessionSuite) TestAutoRedialStopsRetrier(c *C) {
	// checks that AutoRedial stops the previous retrier
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.doneCh = make(chan uint32)
	c.Check(sess.retrier, IsNil)
	sess.autoRedial()
	c.Assert(sess.retrier, NotNil)
	sess.retrier.Stop()
	c.Check(<-sess.doneCh, Not(Equals), 0)
}

func (cs *clientSessionSuite) TestAutoRedialCallsRedialDelay(c *C) {
	// NOTE there are tests that use calling redialDelay as an indication of calling autoRedial!
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	flag := false
	sess.redialDelay = func(sess *clientSession) time.Duration { flag = true; return 0 }
	sess.autoRedial()
	c.Check(flag, Equals, true)
}

func (cs *clientSessionSuite) TestAutoRedialSetsRedialDelayIfTooQuick(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.redialDelay = func(sess *clientSession) time.Duration { return 0 }
	sess.autoRedial()
	c.Check(sess.ShouldDelay(), Equals, false)
	sess.stopRedial()
	sess.clearShouldDelay()
	sess.autoRedial()
	c.Check(sess.ShouldDelay(), Equals, true)
}

/****************************************************************
  handlePing() tests
****************************************************************/

type msgSuite struct {
	sess   *clientSession
	upCh   chan interface{}
	downCh chan interface{}
}

var _ = Suite(&msgSuite{})

func (s *msgSuite) SetUpTest(c *C) {
	var err error
	conf := dummyConf()
	conf.ExchangeTimeout = time.Millisecond
	s.sess, err = NewSession("", conf, "wah", seenstate.NewSeenState, helpers.NewTestLogger(c, "debug"))
	c.Assert(err, IsNil)
	s.sess.Connection = &testConn{Name: "TestHandle*"}
	s.upCh = make(chan interface{}, 5)
	s.downCh = make(chan interface{}, 5)
	s.sess.proto = &testProtocol{up: s.upCh, down: s.downCh}
}

func (s *msgSuite) TestHandlePingWorks(c *C) {
	s.upCh <- nil // no error
	c.Check(s.sess.handlePing(), IsNil)
	c.Assert(len(s.downCh), Equals, 1)
	c.Check(<-s.downCh, Equals, protocol.PingPongMsg{Type: "pong"})
}

func (s *msgSuite) TestHandlePingHandlesPongWriteError(c *C) {
	failure := errors.New("Pong")
	s.upCh <- failure

	c.Check(s.sess.handlePing(), Equals, failure)
	c.Assert(len(s.downCh), Equals, 1)
	c.Check(<-s.downCh, Equals, protocol.PingPongMsg{Type: "pong"})
	c.Check(s.sess.State(), Equals, Error)
}

func (s *msgSuite) TestHandlePingClearsDelay(c *C) {
	s.sess.setShouldDelay()
	s.upCh <- nil // no error
	c.Check(s.sess.handlePing(), IsNil)
	c.Assert(len(s.downCh), Equals, 1)
	c.Check(<-s.downCh, Equals, protocol.PingPongMsg{Type: "pong"})
	c.Check(s.sess.ShouldDelay(), Equals, false)
}

func (s *msgSuite) TestHandlePingDoesNotClearsDelayOnError(c *C) {
	s.sess.setShouldDelay()
	s.upCh <- errors.New("Pong")
	c.Check(s.sess.handlePing(), NotNil)
	c.Assert(len(s.downCh), Equals, 1)
	c.Check(<-s.downCh, Equals, protocol.PingPongMsg{Type: "pong"})
	c.Check(s.sess.ShouldDelay(), Equals, true)
}

/****************************************************************
  handleBroadcast() tests
****************************************************************/

func (s *msgSuite) TestHandleBroadcastWorks(c *C) {
	msg := new(serverMsg)
	msg.Type = "broadcast"
	msg.BroadcastMsg = protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "--ignored--",
		ChanId:   "0",
		TopLevel: 2,
		Payloads: []json.RawMessage{
			json.RawMessage(`{"img1/m1":[101,"tubular"]}`),
			json.RawMessage("false"), // shouldn't happen but robust
			json.RawMessage(`{"img1/m1":[102,"tubular"]}`),
		},
	}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, Equals, nil)
	c.Assert(len(s.sess.BroadcastCh), Equals, 1)
	c.Check(<-s.sess.BroadcastCh, DeepEquals, &BroadcastNotification{
		TopLevel: 2,
		Decoded: []map[string]interface{}{
			map[string]interface{}{
				"img1/m1": []interface{}{float64(101), "tubular"},
			},
			map[string]interface{}{
				"img1/m1": []interface{}{float64(102), "tubular"},
			},
		},
	})
	// and finally, the session keeps track of the levels
	levels, err := s.sess.SeenState.GetAllLevels()
	c.Check(err, IsNil)
	c.Check(levels, DeepEquals, map[string]int64{"0": 2})
}

func (s *msgSuite) TestHandleBroadcastBadAckWrite(c *C) {
	msg := new(serverMsg)
	msg.Type = "broadcast"
	msg.BroadcastMsg = protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "APP",
		ChanId:   "0",
		TopLevel: 2,
		Payloads: []json.RawMessage{json.RawMessage(`{"b":1}`)},
	}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	failure := errors.New("ACK ACK ACK")
	s.upCh <- failure
	c.Assert(<-s.sess.errCh, Equals, failure)
	c.Check(s.sess.State(), Equals, Error)
}

func (s *msgSuite) TestHandleBroadcastWrongChannel(c *C) {
	msg := new(serverMsg)
	msg.Type = "brodacast"
	msg.BroadcastMsg = protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "APP",
		ChanId:   "something awful",
		TopLevel: 2,
		Payloads: []json.RawMessage{json.RawMessage(`{"b":1}`)},
	}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, IsNil)
	c.Check(len(s.sess.BroadcastCh), Equals, 0)
}

func (s *msgSuite) TestHandleBroadcastBrokenSeenState(c *C) {
	s.sess.SeenState = &brokenSeenState{}
	msg := new(serverMsg)
	msg.Type = "broadcast"
	msg.BroadcastMsg = protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "--ignored--",
		ChanId:   "0",
		TopLevel: 2,
		Payloads: []json.RawMessage{json.RawMessage(`{"b":1}`)},
	}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	s.upCh <- nil // ack ok
	// start returns with error
	c.Check(<-s.sess.errCh, Not(Equals), nil)
	c.Check(s.sess.State(), Equals, Error)
	// no message sent out
	c.Check(len(s.sess.BroadcastCh), Equals, 0)
	// and nak'ed it
	c.Check(len(s.downCh), Equals, 1)
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"nak"})
}

func (s *msgSuite) TestHandleBroadcastClearsDelay(c *C) {
	s.sess.setShouldDelay()

	msg := &serverMsg{Type: "broadcast"}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, IsNil)

	c.Check(s.sess.ShouldDelay(), Equals, false)
}

func (s *msgSuite) TestHandleBroadcastDoesNotClearDelayOnError(c *C) {
	s.sess.setShouldDelay()

	msg := &serverMsg{Type: "broadcast"}
	go func() { s.sess.errCh <- s.sess.handleBroadcast(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- errors.New("bcast")
	c.Check(<-s.sess.errCh, NotNil)

	c.Check(s.sess.ShouldDelay(), Equals, true)
}

/****************************************************************
  handleNotifications() tests
****************************************************************/

type testAddresseeChecking struct {
	ops     chan string
	missing string
}

func (ac *testAddresseeChecking) StartAddresseeBatch() {
	ac.ops <- "start"
}

func (ac *testAddresseeChecking) CheckForAddressee(notif *protocol.Notification) *click.AppId {
	ac.ops <- notif.AppId
	if notif.AppId != ac.missing {
		id, err := click.ParseAppId(notif.AppId)
		if err != nil {
			panic(err)
		}
		return id
	} else {
		return nil
	}
}

func (s *msgSuite) TestHandleNotificationsWorks(c *C) {
	ac := &testAddresseeChecking{ops: make(chan string, 10)}
	s.sess.AddresseeChecker = ac
	s.sess.setShouldDelay()
	n1 := protocol.Notification{
		AppId:   "com.example.app1_app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	n2 := protocol.Notification{
		AppId:   "com.example.app2_app2",
		MsgId:   "b",
		Payload: json.RawMessage(`{"m": 2}`),
	}
	msg := new(serverMsg)
	msg.Type = "notifications"
	msg.NotificationsMsg = protocol.NotificationsMsg{
		Notifications: []protocol.Notification{n1, n2},
	}
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, Equals, nil)
	c.Check(s.sess.ShouldDelay(), Equals, false)
	c.Assert(s.sess.NotificationsCh, HasLen, 2)
	app1, err := click.ParseAppId("com.example.app1_app1")
	c.Assert(err, IsNil)
	c.Check(<-s.sess.NotificationsCh, DeepEquals, AddressedNotification{
		To:           app1,
		Notification: &n1,
	})
	app2, err := click.ParseAppId("com.example.app2_app2")
	c.Assert(err, IsNil)
	c.Check(<-s.sess.NotificationsCh, DeepEquals, AddressedNotification{
		To:           app2,
		Notification: &n2,
	})
	c.Check(ac.ops, HasLen, 3)
	c.Check(<-ac.ops, Equals, "start")
	c.Check(<-ac.ops, Equals, "com.example.app1_app1")
	c.Check(<-ac.ops, Equals, "com.example.app2_app2")
}

func (s *msgSuite) TestHandleNotificationsAddresseeCheck(c *C) {
	ac := &testAddresseeChecking{
		ops:     make(chan string, 10),
		missing: "com.example.app1_app1",
	}
	s.sess.AddresseeChecker = ac
	s.sess.setShouldDelay()
	n1 := protocol.Notification{
		AppId:   "com.example.app1_app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	n2 := protocol.Notification{
		AppId:   "com.example.app2_app2",
		MsgId:   "b",
		Payload: json.RawMessage(`{"m": 2}`),
	}
	msg := new(serverMsg)
	msg.Type = "notifications"
	msg.NotificationsMsg = protocol.NotificationsMsg{
		Notifications: []protocol.Notification{n1, n2},
	}
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, Equals, nil)
	c.Check(s.sess.ShouldDelay(), Equals, false)
	c.Assert(s.sess.NotificationsCh, HasLen, 1)
	app2, err := click.ParseAppId("com.example.app2_app2")
	c.Assert(err, IsNil)
	c.Check(<-s.sess.NotificationsCh, DeepEquals, AddressedNotification{
		To:           app2,
		Notification: &n2,
	})
	c.Check(ac.ops, HasLen, 3)
	c.Check(<-ac.ops, Equals, "start")
	c.Check(<-ac.ops, Equals, "com.example.app1_app1")
}

func (s *msgSuite) TestHandleNotificationsFiltersSeen(c *C) {
	ac := &testAddresseeChecking{ops: make(chan string, 10)}
	s.sess.AddresseeChecker = ac
	n1 := protocol.Notification{
		AppId:   "com.example.app1_app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	n2 := protocol.Notification{
		AppId:   "com.example.app2_app2",
		MsgId:   "b",
		Payload: json.RawMessage(`{"m": 2}`),
	}
	msg := new(serverMsg)
	msg.Type = "notifications"
	msg.NotificationsMsg = protocol.NotificationsMsg{
		Notifications: []protocol.Notification{n1, n2},
	}
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, Equals, nil)
	c.Assert(s.sess.NotificationsCh, HasLen, 2)
	app1, err := click.ParseAppId("com.example.app1_app1")
	c.Assert(err, IsNil)
	c.Check(<-s.sess.NotificationsCh, DeepEquals, AddressedNotification{
		To:           app1,
		Notification: &n1,
	})
	app2, err := click.ParseAppId("com.example.app2_app2")
	c.Assert(err, IsNil)
	c.Check(<-s.sess.NotificationsCh, DeepEquals, AddressedNotification{
		To:           app2,
		Notification: &n2,
	})
	c.Check(ac.ops, HasLen, 3)

	// second time they get ignored
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	s.upCh <- nil // ack ok
	c.Check(<-s.sess.errCh, Equals, nil)
	c.Assert(s.sess.NotificationsCh, HasLen, 0)
	c.Check(ac.ops, HasLen, 4)
}

func (s *msgSuite) TestHandleNotificationsBadAckWrite(c *C) {
	s.sess.setShouldDelay()
	n1 := protocol.Notification{
		AppId:   "com.example.app1_app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	msg := new(serverMsg)
	msg.Type = "notifications"
	msg.NotificationsMsg = protocol.NotificationsMsg{
		Notifications: []protocol.Notification{n1},
	}
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	failure := errors.New("ACK ACK ACK")
	s.upCh <- failure
	c.Assert(<-s.sess.errCh, Equals, failure)
	c.Check(s.sess.State(), Equals, Error)
	// didn't get to clear
	c.Check(s.sess.ShouldDelay(), Equals, true)
}

func (s *msgSuite) TestHandleNotificationsBrokenSeenState(c *C) {
	s.sess.setShouldDelay()
	s.sess.SeenState = &brokenSeenState{}
	n1 := protocol.Notification{
		AppId:   "com.example.app1_app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	msg := new(serverMsg)
	msg.Type = "notifications"
	msg.NotificationsMsg = protocol.NotificationsMsg{
		Notifications: []protocol.Notification{n1},
	}
	go func() { s.sess.errCh <- s.sess.handleNotifications(msg) }()
	s.upCh <- nil // ack ok
	// start returns with error
	c.Check(<-s.sess.errCh, Not(Equals), nil)
	c.Check(s.sess.State(), Equals, Error)
	// no message sent out
	c.Check(len(s.sess.NotificationsCh), Equals, 0)
	// and nak'ed it
	c.Check(len(s.downCh), Equals, 1)
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"nak"})
	// didn't get to clear
	c.Check(s.sess.ShouldDelay(), Equals, true)
}

/****************************************************************
  handleConnBroken() tests
****************************************************************/

func (s *msgSuite) TestHandleConnBrokenUnkwown(c *C) {
	msg := new(serverMsg)
	msg.Type = "connbroken"
	msg.ConnBrokenMsg = protocol.ConnBrokenMsg{
		Reason: "REASON",
	}
	go func() { s.sess.errCh <- s.sess.handleConnBroken(msg) }()
	c.Check(<-s.sess.errCh, ErrorMatches, "server broke connection: REASON")
	c.Check(s.sess.State(), Equals, Error)
}

func (s *msgSuite) TestHandleConnBrokenHostMismatch(c *C) {
	msg := new(serverMsg)
	msg.Type = "connbroken"
	msg.ConnBrokenMsg = protocol.ConnBrokenMsg{
		Reason: protocol.BrokenHostMismatch,
	}
	s.sess.deliveryHosts = []string{"foo:443", "bar:443"}
	go func() { s.sess.errCh <- s.sess.handleConnBroken(msg) }()
	c.Check(<-s.sess.errCh, ErrorMatches, "server broke connection: host-mismatch")
	c.Check(s.sess.State(), Equals, Error)
	// hosts were reset
	c.Check(s.sess.deliveryHosts, IsNil)
}

/****************************************************************
  loop() tests
****************************************************************/

type loopSuite msgSuite

var _ = Suite(&loopSuite{})

func (s *loopSuite) SetUpTest(c *C) {
	(*msgSuite)(s).SetUpTest(c)
	s.sess.Connection.(*testConn).Name = "TestLoop*"
	go func() {
		s.sess.errCh <- s.sess.loop()
	}()
}

func (s *loopSuite) waitUntilRunning(c *C) {
	delay := time.Duration(5000)
	for i := 0; i < 5; i++ {
		if s.sess.State() == Running {
			return
		}
		time.Sleep(delay)
		delay *= 2
	}
	c.Check(s.sess.State(), Equals, Running)
}

func (s *loopSuite) TestLoopReadError(c *C) {
	s.waitUntilRunning(c)
	s.upCh <- errors.New("Read")
	err := <-s.sess.errCh
	c.Check(err, ErrorMatches, "Read")
	c.Check(s.sess.State(), Equals, Error)
}

func (s *loopSuite) TestLoopPing(c *C) {
	s.waitUntilRunning(c)
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	s.upCh <- protocol.PingPongMsg{Type: "ping"}
	c.Check(takeNext(s.downCh), Equals, protocol.PingPongMsg{Type: "pong"})
	failure := errors.New("pong")
	s.upCh <- failure
	c.Check(<-s.sess.errCh, Equals, failure)
}

func (s *loopSuite) TestLoopLoopsDaLoop(c *C) {
	s.waitUntilRunning(c)
	for i := 1; i < 10; i++ {
		c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
		s.upCh <- protocol.PingPongMsg{Type: "ping"}
		c.Check(takeNext(s.downCh), Equals, protocol.PingPongMsg{Type: "pong"})
		s.upCh <- nil
	}
	failure := errors.New("pong")
	s.upCh <- failure
	c.Check(<-s.sess.errCh, Equals, failure)
}

func (s *loopSuite) TestLoopBroadcast(c *C) {
	s.waitUntilRunning(c)
	b := &protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "--ignored--",
		ChanId:   "0",
		TopLevel: 2,
		Payloads: []json.RawMessage{json.RawMessage(`{"b":1}`)},
	}
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	s.upCh <- b
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	failure := errors.New("ack")
	s.upCh <- failure
	c.Check(<-s.sess.errCh, Equals, failure)
}

func (s *loopSuite) TestLoopNotifications(c *C) {
	s.waitUntilRunning(c)

	n1 := protocol.Notification{
		AppId:   "app1",
		MsgId:   "a",
		Payload: json.RawMessage(`{"m": 1}`),
	}
	msg := &protocol.NotificationsMsg{
		Type:          "notifications",
		Notifications: []protocol.Notification{n1},
	}
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	s.upCh <- msg
	c.Check(takeNext(s.downCh), Equals, protocol.AckMsg{"ack"})
	failure := errors.New("ack")
	s.upCh <- failure
	c.Check(<-s.sess.errCh, Equals, failure)
}

func (s *loopSuite) TestLoopSetParams(c *C) {
	s.waitUntilRunning(c)
	setParams := protocol.SetParamsMsg{
		Type:      "setparams",
		SetCookie: "COOKIE",
	}
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	s.upCh <- setParams
	failure := errors.New("fail")
	s.upCh <- failure
	c.Assert(<-s.sess.errCh, Equals, failure)
	c.Check(s.sess.getCookie(), Equals, "COOKIE")
}

func (s *loopSuite) TestLoopConnBroken(c *C) {
	s.waitUntilRunning(c)
	broken := protocol.ConnBrokenMsg{
		Type:   "connbroken",
		Reason: "REASON",
	}
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	s.upCh <- broken
	c.Check(<-s.sess.errCh, NotNil)
}

func (s *loopSuite) TestLoopConnWarn(c *C) {
	warn := protocol.ConnWarnMsg{
		Type:   "warn",
		Reason: "XXX",
	}
	connwarn := protocol.ConnWarnMsg{
		Type:   "connwarn",
		Reason: "REASON",
	}
	failure := errors.New("warn")
	log := s.sess.Log.(*helpers.TestLogger)

	s.waitUntilRunning(c)
	c.Check(takeNext(s.downCh), Equals, "deadline 1ms")
	log.ResetCapture()
	s.upCh <- warn
	s.upCh <- connwarn
	s.upCh <- failure
	c.Check(<-s.sess.errCh, Equals, failure)
	c.Check(log.Captured(),
		Matches, `(?ms).* warning: XXX$.*`)
	c.Check(log.Captured(),
		Matches, `(?ms).* warning: REASON$`)
}

/****************************************************************
  start() tests
****************************************************************/
func (cs *clientSessionSuite) TestStartFailsIfSetDeadlineFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartFailsIfSetDeadlineFails",
		DeadlineCondition: condition.Work(false)} // setdeadline will fail
	err = sess.start()
	c.Check(err, ErrorMatches, ".*deadline.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartFailsIfWriteFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartFailsIfWriteFails",
		WriteCondition: condition.Work(false)} // write will fail
	err = sess.start()
	c.Check(err, ErrorMatches, ".*write.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartFailsIfGetLevelsFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.SeenState = &brokenSeenState{}
	sess.Connection = &testConn{Name: "TestStartConnectMessageFails"}
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	err = <-errCh
	c.Check(err, ErrorMatches, "broken.")
}

func (cs *clientSessionSuite) TestStartConnectMessageFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartConnectMessageFails"}
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	c.Check(takeNext(downCh), DeepEquals, protocol.ConnectMsg{
		Type:          "connect",
		DeviceId:      sess.DeviceId,
		Levels:        map[string]int64{},
	})
	upCh <- errors.New("Overflow error in /dev/null")
	err = <-errCh
	c.Check(err, ErrorMatches, "Overflow.*null")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartConnackReadError(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartConnackReadError"}
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	_, ok := takeNext(downCh).(protocol.ConnectMsg)
	c.Check(ok, Equals, true)
	upCh <- nil // no error
	upCh <- io.EOF
	err = <-errCh
	c.Check(err, ErrorMatches, ".*EOF.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartBadConnack(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartBadConnack"}
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	_, ok := takeNext(downCh).(protocol.ConnectMsg)
	c.Check(ok, Equals, true)
	upCh <- nil // no error
	upCh <- protocol.ConnAckMsg{Type: "connack"}
	err = <-errCh
	c.Check(err, ErrorMatches, ".*invalid.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartNotConnack(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartBadConnack"}
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	_, ok := takeNext(downCh).(protocol.ConnectMsg)
	c.Check(ok, Equals, true)
	upCh <- nil // no error
	upCh <- protocol.ConnAckMsg{Type: "connnak"}
	err = <-errCh
	c.Check(err, ErrorMatches, ".*CONNACK.*")
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestStartWorks(c *C) {
	info := map[string]interface{}{
		"foo": 1,
		"bar": "baz",
	}
	conf := ClientSessionConfig{
		Info: info,
	}
	sess, err := NewSession("", conf, "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Connection = &testConn{Name: "TestStartWorks"}
	sess.setCookie("COOKIE")
	errCh := make(chan error, 1)
	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(_ net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.start()
	}()

	c.Check(takeNext(downCh), Equals, "deadline 0")
	msg, ok := takeNext(downCh).(protocol.ConnectMsg)
	c.Check(ok, Equals, true)
	c.Check(msg.DeviceId, Equals, "wah")
	c.Check(msg.Cookie, Equals, "COOKIE")
	c.Check(msg.Info, DeepEquals, info)
	upCh <- nil // no error
	upCh <- protocol.ConnAckMsg{
		Type:   "connack",
		Params: protocol.ConnAckParams{(10 * time.Millisecond).String()},
	}
	// start is now done.
	err = <-errCh
	c.Check(err, IsNil)
	c.Check(sess.State(), Equals, Started)
}

/****************************************************************
  run() tests
****************************************************************/

func (cs *clientSessionSuite) TestRunCallsCloserWithFalse(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	failure := errors.New("bail")
	has_closed := false
	with_false := false
	err = sess.run(
		func(b bool) { has_closed = true; with_false = !b },
		func() error { return failure },
		nil,
		nil,
		nil)
	c.Check(err, Equals, failure)
	c.Check(has_closed, Equals, true)
	c.Check(with_false, Equals, true)
}

func (cs *clientSessionSuite) TestRunBailsIfHostGetterFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	failure := errors.New("TestRunBailsIfHostGetterFails")
	has_closed := false
	err = sess.run(
		func(bool) { has_closed = true },
		func() error { return failure },
		nil,
		nil,
		nil)
	c.Check(err, Equals, failure)
	c.Check(has_closed, Equals, true)
}

func (cs *clientSessionSuite) TestRunBailsIfConnectFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	failure := errors.New("TestRunBailsIfConnectFails")
	err = sess.run(
		func(bool) {},
		func() error { return nil },
		func() error { return failure },
		nil,
		nil)
	c.Check(err, Equals, failure)
}

func (cs *clientSessionSuite) TestRunBailsIfStartFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	failure := errors.New("TestRunBailsIfStartFails")
	err = sess.run(
		func(bool) {},
		func() error { return nil },
		func() error { return nil },
		func() error { return failure },
		nil)
	c.Check(err, Equals, failure)
}

func (cs *clientSessionSuite) TestRunRunsEvenIfLoopFails(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	failureCh := make(chan error) // must be unbuffered
	notf := &BroadcastNotification{}
	err = sess.run(
		func(bool) {},
		func() error { return nil },
		func() error { return nil },
		func() error { return nil },
		func() error { sess.BroadcastCh <- notf; return <-failureCh })
	c.Check(err, Equals, nil)
	// if run doesn't error it sets up the channels
	c.Assert(sess.errCh, NotNil)
	c.Assert(sess.BroadcastCh, NotNil)
	c.Check(<-sess.BroadcastCh, Equals, notf)
	failure := errors.New("TestRunRunsEvenIfLoopFails")
	failureCh <- failure
	c.Check(<-sess.errCh, Equals, failure)
	// so now you know it was running in a goroutine :)
}

/****************************************************************
  Jitter() tests
****************************************************************/

func (cs *clientSessionSuite) TestJitter(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	num_tries := 20       // should do the math
	spread := time.Second //
	has_neg := false
	has_pos := false
	has_zero := true
	for i := 0; i < num_tries; i++ {
		n := sess.Jitter(spread)
		if n > 0 {
			has_pos = true
		} else if n < 0 {
			has_neg = true
		} else {
			has_zero = true
		}
	}
	c.Check(has_neg, Equals, true)
	c.Check(has_pos, Equals, true)
	c.Check(has_zero, Equals, true)

	// a negative spread is caught in the reasonable place
	c.Check(func() { sess.Jitter(time.Duration(-1)) }, PanicMatches,
		"spread must be non-negative")
}

/****************************************************************
  Dial() tests
****************************************************************/

func (cs *clientSessionSuite) TestDialPanics(c *C) {
	// one last unhappy test
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.Protocolator = nil
	c.Check(sess.Dial, PanicMatches, ".*protocol constructor.")
}

var (
	dialTestTimeout = 300 * time.Millisecond
)

func dialTestConf(certPEM []byte) ClientSessionConfig {
	conf := dummyConf()
	conf.ExchangeTimeout = dialTestTimeout
	if certPEM == nil {
		conf.PEM = helpers.TestCertPEMBlock
	} else {
		conf.PEM = certPEM
	}
	return conf
}

func (cs *clientSessionSuite) TestDialBadServerName(c *C) {
	// a borked server name
	lst, err := tls.Listen("tcp", "localhost:0", helpers.TestTLSServerConfig)
	c.Assert(err, IsNil)
	// advertise
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(map[string]interface{}{
			"domain": "xyzzy", // <-- *** THIS *** is the bit that'll break it
			"hosts":  []string{"nowhere", lst.Addr().String()},
		})
		if err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer ts.Close()

	sess, err := NewSession(ts.URL, dialTestConf(nil), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	tconn := &testConn{}
	sess.Connection = tconn

	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	errCh := make(chan error, 1)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(net.Conn) protocol.Protocol { return proto }

	go func() {
		errCh <- sess.Dial()
	}()

	srv, err := lst.Accept()
	c.Assert(err, IsNil)

	// connect done

	_, err = protocol.ReadWireFormatVersion(srv, dialTestTimeout)
	c.Check(err, NotNil)

	c.Check(<-errCh, NotNil)
	c.Check(sess.State(), Equals, Error)
}

func (cs *clientSessionSuite) TestDialWorks(c *C) {
	// happy path thoughts
	lst, err := tls.Listen("tcp", "localhost:0", helpers.TestTLSServerConfig)
	c.Assert(err, IsNil)
	// advertise
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(map[string]interface{}{
			"domain": "push-delivery",
			"hosts":  []string{"nowhere", lst.Addr().String()},
		})
		if err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
	defer ts.Close()

	sess, err := NewSession(ts.URL, dialTestConf(nil), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	tconn := &testConn{CloseCondition: condition.Fail2Work(10)}
	sess.Connection = tconn
	// just to be sure:
	c.Check(tconn.CloseCondition.String(), Matches, ".* 10 to go.")

	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(net.Conn) protocol.Protocol { return proto }

	go sess.Dial()

	srv, err := lst.Accept()
	c.Assert(err, IsNil)

	// connect done

	// Dial should have had the session's old connection (tconn) closed
	// before connecting a new one; if that was done, tconn's condition
	// ticked forward:
	c.Check(tconn.CloseCondition.String(), Matches, ".* 9 to go.")

	// now, start: 1. protocol version
	v, err := protocol.ReadWireFormatVersion(srv, dialTestTimeout)
	c.Assert(err, IsNil)
	c.Assert(v, Equals, protocol.ProtocolWireVersion)

	// if something goes wrong session would try the first/other host
	c.Check(sess.tryHost, Equals, 0)

	// 2. "connect" (but on the fake protcol above! woo)

	c.Check(takeNext(downCh), Equals, fmt.Sprintf("deadline %v", dialTestTimeout))
	_, ok := takeNext(downCh).(protocol.ConnectMsg)
	c.Check(ok, Equals, true)
	upCh <- nil // no error
	upCh <- protocol.ConnAckMsg{
		Type:   "connack",
		Params: protocol.ConnAckParams{(10 * time.Millisecond).String()},
	}
	// start is now done.

	// 3. "loop"

	// ping works,
	c.Check(takeNext(downCh), Equals, fmt.Sprintf("deadline %v", dialTestTimeout+10*time.Millisecond))
	upCh <- protocol.PingPongMsg{Type: "ping"}
	c.Check(takeNext(downCh), Equals, protocol.PingPongMsg{Type: "pong"})
	upCh <- nil

	// session would retry the same host
	c.Check(sess.tryHost, Equals, 1)

	// and broadcasts...
	b := &protocol.BroadcastMsg{
		Type:     "broadcast",
		AppId:    "--ignored--",
		ChanId:   "0",
		TopLevel: 2,
		Payloads: []json.RawMessage{json.RawMessage(`{"b":1}`)},
	}
	c.Check(takeNext(downCh), Equals, fmt.Sprintf("deadline %v", dialTestTimeout+10*time.Millisecond))
	upCh <- b
	c.Check(takeNext(downCh), Equals, protocol.AckMsg{"ack"})
	upCh <- nil
	// ...get bubbled up,
	c.Check(<-sess.BroadcastCh, NotNil)
	// and their TopLevel remembered
	levels, err := sess.SeenState.GetAllLevels()
	c.Check(err, IsNil)
	c.Check(levels, DeepEquals, map[string]int64{"0": 2})

	// and ping still work even after that.
	c.Check(takeNext(downCh), Equals, fmt.Sprintf("deadline %v", dialTestTimeout+10*time.Millisecond))
	upCh <- protocol.PingPongMsg{Type: "ping"}
	c.Check(takeNext(downCh), Equals, protocol.PingPongMsg{Type: "pong"})
	failure := errors.New("pongs")
	upCh <- failure
	c.Check(<-sess.errCh, Equals, failure)
}

func (cs *clientSessionSuite) TestDialWorksDirect(c *C) {
	// happy path thoughts
	lst, err := tls.Listen("tcp", "localhost:0", helpers.TestTLSServerConfig)
	c.Assert(err, IsNil)
	sess, err := NewSession(lst.Addr().String(), dialTestConf(nil), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	defer sess.StopKeepConnection()

	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(net.Conn) protocol.Protocol { return proto }

	go sess.Dial()

	cli, err := lst.Accept()
	c.Assert(err, IsNil)
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1]byte
	_, err = cli.Read(buf[:])
	c.Assert(err, IsNil)
	// connect done
}

func (cs *clientSessionSuite) TestDialWorksDirectSHA512Cert(c *C) {
	// happy path thoughts
	lst, err := tls.Listen("tcp", "localhost:0", helpers.TestTLSServerConfigs["sha512"])
	c.Assert(err, IsNil)
	sess, err := NewSession(lst.Addr().String(), dialTestConf(helpers.TestCertPEMBlock512), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	defer sess.StopKeepConnection()

	upCh := make(chan interface{}, 5)
	downCh := make(chan interface{}, 5)
	proto := &testProtocol{up: upCh, down: downCh}
	sess.Protocolator = func(conn net.Conn) protocol.Protocol {
		return proto
	}

	go sess.Dial()

	cli, err := lst.Accept()
	c.Assert(err, IsNil)
	cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1]byte
	_, err = cli.Read(buf[:])
	c.Assert(err, IsNil)
	// connect done
}

/****************************************************************
  redialDelay() tests
****************************************************************/

func (cs *clientSessionSuite) TestShouldDelay(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Check(sess.ShouldDelay(), Equals, false)
	sess.setShouldDelay()
	c.Check(sess.ShouldDelay(), Equals, true)
	sess.clearShouldDelay()
	c.Check(sess.ShouldDelay(), Equals, false)
}

func (cs *clientSessionSuite) TestRedialDelay(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.redialDelays = []time.Duration{17, 42}
	n := 0
	sess.redialJitter = func(time.Duration) time.Duration { n++; return 0 }
	// we get increasing delays while we're unhappy
	sess.setShouldDelay()
	c.Check(redialDelay(sess), Equals, time.Duration(17))
	c.Check(redialDelay(sess), Equals, time.Duration(42))
	c.Check(redialDelay(sess), Equals, time.Duration(42))
	// once we're happy, delays drop to 0
	sess.clearShouldDelay()
	c.Check(redialDelay(sess), Equals, time.Duration(0))
	// and start again from the top if we become unhappy again
	sess.setShouldDelay()
	c.Check(redialDelay(sess), Equals, time.Duration(17))
	// and redialJitter got called every time shouldDelay was true
	c.Check(n, Equals, 4)
}

/****************************************************************
  ResetCookie() tests
****************************************************************/

func (cs *clientSessionSuite) TestResetCookie(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Assert(sess.KeepConnection(), IsNil)
	defer sess.StopKeepConnection()
	c.Check(sess.getCookie(), Equals, "")
	sess.setCookie("COOKIE")
	c.Check(sess.getCookie(), Equals, "COOKIE")
	sess.ResetCookie()
	c.Check(sess.getCookie(), Equals, "")
}

/****************************************************************
  KeepConnection() (and related) tests
****************************************************************/

func (cs *clientSessionSuite) TestKeepConnectionDoesNothingIfNotConnected(c *C) {
	// how do you test "does nothing?"
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Assert(sess, NotNil)
	c.Assert(sess.State(), Equals, Pristine)
	c.Assert(sess.KeepConnection(), IsNil)
	defer sess.StopKeepConnection()
	// stopCh is meant to be used just for closing it, but abusing
	// it for testing seems the right thing to do: this ensures
	// the thing is ticking along before we check the state of
	// stuff.
	sess.stopCh <- struct{}{}
	c.Check(sess.State(), Equals, Disconnected)
}

func (cs *clientSessionSuite) TestYouCantCallKeepConnectionTwice(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Assert(sess, NotNil)
	c.Assert(sess.State(), Equals, Pristine)
	c.Assert(sess.KeepConnection(), IsNil)
	defer sess.StopKeepConnection()
	c.Check(sess.KeepConnection(), NotNil)
}

func (cs *clientSessionSuite) TestStopKeepConnectionShutsdown(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Assert(sess, NotNil)
	sess.StopKeepConnection()
	c.Check(sess.State(), Equals, Shutdown)
}

func (cs *clientSessionSuite) TestHasConnectivityTriggersConnectivityHandler(c *C) {
	sess, err := NewSession("foo:443", dummyConf(), "", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	c.Assert(sess, NotNil)
	testCh := make(chan bool)
	sess.connHandler = func(p bool) { testCh <- p }
	go sess.doKeepConnection()
	defer sess.StopKeepConnection()
	sess.HasConnectivity(true)
	c.Check(<-testCh, Equals, true)
	sess.HasConnectivity(false)
	c.Check(<-testCh, Equals, false)
}

func (cs *clientSessionSuite) TestDoneChIsEmptiedAndLogged(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	sess.doneCh = make(chan uint32) // unbuffered

	sess.KeepConnection()
	defer sess.StopKeepConnection()

	sess.doneCh <- 23
	sess.doneCh <- 24 // makes sure the first one has been processed before checking

	c.Check(cs.log.Captured(),
		Matches, `(?ms).* connected after 23 attempts\.`)
}

func (cs *clientSessionSuite) TestErrChIsEmptiedAndLoggedAndAutoRedial(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.errCh = make(chan error) // unbuffered
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.lastConn = true // -> autoRedial, if the session is in Disconnected

	sess.KeepConnection()
	defer sess.StopKeepConnection()

	sess.setState(Error)
	sess.errCh <- errors.New("potato")
	select {
	case <-ch:
		// all ok
	case <-time.After(100 * time.Millisecond):
		c.Fatalf("redialDelay not called (-> autoRedial not called)?")
	}

	c.Check(cs.log.Captured(),
		Matches, `(?ms).* session error.*potato`)
}

func (cs *clientSessionSuite) TestErrChIsEmptiedAndLoggedNoAutoRedial(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.errCh = make(chan error) // unbuffered
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.connHandler = func(bool) {}
	sess.lastConn = false // so, no autoredial

	sess.KeepConnection()
	defer sess.StopKeepConnection()

	sess.errCh <- errors.New("potato")
	c.Assert(sess.State(), Equals, Disconnected)
	select {
	case <-ch:
		c.Fatalf("redialDelay called (-> autoRedial called) when disconnected?")
	case <-time.After(100 * time.Millisecond):
		// all ok
	}

	c.Check(cs.log.Captured(),
		Matches, `(?ms).* session error.*potato`)
}

func (cs *clientSessionSuite) TestHandleConnConnFromConnected(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.state = Connected
	sess.lastConn = true
	sess.handleConn(true)
	c.Check(sess.lastConn, Equals, true)

	select {
	case <-ch:
		// all ok
	case <-time.After(100 * time.Millisecond):
		c.Fatalf("redialDelay not called (-> autoRedial not called)?")
	}
}

func (cs *clientSessionSuite) TestHandleConnConnFromDisconnected(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.state = Disconnected
	sess.lastConn = false
	sess.handleConn(true)
	c.Check(sess.lastConn, Equals, true)

	select {
	case <-ch:
		// all ok
	case <-time.After(100 * time.Millisecond):
		c.Fatalf("redialDelay not called (-> autoRedial not called)?")
	}
}

func (cs *clientSessionSuite) TestHandleConnNotConnFromDisconnected(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.state = Disconnected
	sess.lastConn = false
	sess.handleConn(false)
	c.Check(sess.lastConn, Equals, false)

	select {
	case <-ch:
		c.Fatalf("redialDelay called (-> autoRedial called)?")
	case <-time.After(100 * time.Millisecond):
		// all ok
	}
	c.Check(cs.log.Captured(), Matches, `(?ms).*-> Disconnected`)
}

func (cs *clientSessionSuite) TestHandleConnNotConnFromConnected(c *C) {
	sess, err := NewSession("", dummyConf(), "wah", cs.lvls, cs.log)
	c.Assert(err, IsNil)
	ch := make(chan struct{}, 1)
	sess.redialDelay = func(sess *clientSession) time.Duration { ch <- struct{}{}; return 0 }
	sess.state = Connected
	sess.lastConn = true
	sess.handleConn(false)
	c.Check(sess.lastConn, Equals, false)

	select {
	case <-ch:
		c.Fatalf("redialDelay called (-> autoRedial called)?")
	case <-time.After(100 * time.Millisecond):
		// all ok
	}
	c.Check(cs.log.Captured(), Matches, `(?ms).*-> Disconnected`)
}
