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
	. "launchpad.net/gocheck"

	"github.com/ubports/ubuntu-push/logger"
	"github.com/ubports/ubuntu-push/server/broker"
	"github.com/ubports/ubuntu-push/server/broker/testsuite"
	"github.com/ubports/ubuntu-push/server/store"
)

// run the common broker test suite against SimpleBroker

// aliasing through embedding to get saner report names by gocheck
type commonBrokerSuite struct {
	testsuite.CommonBrokerSuite
}

// trivial session tracker
type testTracker string

func (t testTracker) SessionId() string {
	return string(t)
}

var _ = Suite(&commonBrokerSuite{testsuite.CommonBrokerSuite{
	MakeBroker: func(sto store.PendingStore, cfg broker.BrokerConfig, log logger.Logger) testsuite.FullBroker {
		return NewSimpleBroker(sto, cfg, log, nil)
	},
	MakeTracker: func(sessionId string) broker.SessionTracker {
		return testTracker(sessionId)
	},
	RevealSession: func(b broker.Broker, deviceId string) broker.BrokerSession {
		return b.(*SimpleBroker).registry[deviceId]
	},
	RevealBroadcastExchange: func(exchg broker.Exchange) *broker.BroadcastExchange {
		return exchg.(*broker.BroadcastExchange)
	},
	RevealUnicastExchange: func(exchg broker.Exchange) *broker.UnicastExchange {
		return exchg.(*broker.UnicastExchange)
	},
}})
