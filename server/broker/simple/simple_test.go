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

package simple

import (
	stdtesting "testing"

	. "launchpad.net/gocheck"

	"github.com/ubports/ubuntu-push/server/broker/testing"
	"github.com/ubports/ubuntu-push/server/store"
)

func TestSimple(t *stdtesting.T) { TestingT(t) }

type simpleSuite struct{}

var _ = Suite(&simpleSuite{})

var testBrokerConfig = &testing.TestBrokerConfig{10, 5}

func (s *simpleSuite) TestNew(c *C) {
	sto := store.NewInMemoryPendingStore()
	b := NewSimpleBroker(sto, testBrokerConfig, nil, nil)
	c.Check(cap(b.sessionCh), Equals, 5)
	c.Check(len(b.registry), Equals, 0)
	c.Check(b.sto, Equals, sto)
}

func (s *simpleSuite) TestSessionInternalChannelId(c *C) {
	sess := &simpleBrokerSession{deviceId: "dev21"}
	c.Check(sess.InternalChannelId(), Equals, store.UnicastInternalChannelId("dev21", "dev21"))
}
