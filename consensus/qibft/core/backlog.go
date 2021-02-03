// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	"gopkg.in/karalabe/cookiejar.v2/collections/prque"
)

var (
	// msgPriority is defined for calculating processing priority to speedup consensus
	// msgPreprepare > msgCommit > msgPrepare
	msgPriority = map[uint64]int{
		msgPreprepare: 1,
		msgCommit:     2,
		msgPrepare:    3,
	}
)

// checkMessage checks the message state
// return errInvalidMessage if the message is invalid
// return errFutureMessage if the message view is larger than current view
// return errOldMessage if the message view is smaller than current view
func (c *core) checkMessage(msgCode uint64, view *View) error {
	if view == nil || view.Sequence == nil || view.Round == nil {
		return errInvalidMessage
	}

	if msgCode == msgRoundChange {
		if view.Sequence.Cmp(c.currentView().Sequence) > 0 {
			return errFutureMessage
		} else if view.Cmp(c.currentView()) < 0 {
			return errOldMessage
		}
		return nil
	}

	if view.Cmp(c.currentView()) > 0 {
		return errFutureMessage
	}

	if view.Cmp(c.currentView()) < 0 {
		return errOldMessage
	}

	switch c.state {
	case StateAcceptRequest:
		// StateAcceptRequest only accepts msgPreprepare and msgRoundChange
		// other messages are future messages
		if msgCode > msgPreprepare {
			return errFutureMessage
		}
		return nil
	case StatePreprepared:
		// StatePreprepared only accepts msgPrepare and msgRoundChange
		// message less than msgPrepare are invalid and greater are future messages
		if msgCode < msgPrepare {
			return errInvalidMessage
		} else if msgCode > msgPrepare {
			return errFutureMessage
		}
		return nil
	case StatePrepared:
		// StatePrepared only accepts msgCommit and msgRoundChange
		// other messages are invalid messages
		if msgCode < msgCommit {
			return errInvalidMessage
		}
		return nil
	case StateCommitted:
		// StateCommit rejects all messages other than msgRoundChange
		return errInvalidMessage
	}
	return nil
}

func (c *core) storeQBFTBacklog(msg QBFTMessage) {
	src := msg.Source()
	logger := c.logger.New("from", src, "state", c.state)

	if src == c.Address() {
		logger.Warn("Backlog from self")
		return
	}

	logger.Trace("Store future message")

	c.backlogsMu.Lock()
	defer c.backlogsMu.Unlock()

	logger.Debug("Retrieving backlog queue", "for", src, "backlogs_size", len(c.backlogs))
	backlog := c.backlogs[src]
	if backlog == nil {
		backlog = prque.New()
	}
	view := msg.View()
	backlog.Push(msg, toPriority(msg.Code(), &view))
	c.backlogs[src] = backlog
}

func (c *core) storeBacklog(msg *message, src istanbul.Validator) {
	logger := c.logger.New("from", src, "state", c.state)

	if src.Address() == c.Address() {
		logger.Warn("Backlog from self")
		return
	}

	logger.Trace("Store future message")

	c.backlogsMu.Lock()
	defer c.backlogsMu.Unlock()

	logger.Debug("Retrieving backlog queue", "for", src.Address(), "backlogs_size", len(c.backlogs))
	backlog := c.backlogs[src.Address()]
	if backlog == nil {
		backlog = prque.New()
	}
	switch msg.Code {
	case msgPreprepare:
		var p *Preprepare
		err := msg.Decode(&p)
		if err == nil {
			backlog.Push(msg, toPriority(msg.Code, p.View))
		}
	case msgRoundChange:
		var p *RoundChangeMessage
		err := msg.Decode(&p)
		if err == nil {
			backlog.Push(msg, toPriority(msg.Code, p.View))
		}
		// for msgPrepare and msgCommit cases
	default:
		var p *Subject
		err := msg.Decode(&p)
		if err == nil {
			backlog.Push(msg, toPriority(msg.Code, p.View))
		}
	}
	c.backlogs[src.Address()] = backlog
}

func (c *core) processBacklog() {
	c.backlogsMu.Lock()
	defer c.backlogsMu.Unlock()

	for srcAddress, backlog := range c.backlogs {
		if backlog == nil {
			continue
		}
		_, src := c.valSet.GetByAddress(srcAddress)
		if src == nil {
			// validator is not available
			delete(c.backlogs, srcAddress)
			continue
		}
		logger := c.logger.New("from", src, "state", c.state)
		isFuture := false

		// We stop processing if
		//   1. backlog is empty
		//   2. The first message in queue is a future message
		for !(backlog.Empty() || isFuture) {
			m, prio := backlog.Pop()

			var code uint64
			var view View
			var event backlogEvent

			switch m.(type) {
			// New QBFTMessage processing
			case QBFTMessage:
				msg := m.(QBFTMessage)
				code = msg.Code()
				view = msg.View()
				event.msg = msg
			// old message processing
			case *message:
				msg := m.(*message)
				code = msg.Code
				switch code {
				case msgPreprepare:
					var m *Preprepare
					err := msg.Decode(&m)
					if err == nil {
						view = *m.View
					}
				case msgRoundChange:
					var rc *RoundChangeMessage
					err := msg.Decode(&rc)
					if err == nil {
						view = *rc.View
					}
					// for msgPrepare and msgCommit cases
				default:
					var sub *Subject
					err := msg.Decode(&sub)
					if err == nil {
						view = *sub.View
					}
				}
				event.msg = msg
			}

			// Push back if it's a future message
			err := c.checkMessage(code, &view)
			if err != nil {
				if err == errFutureMessage {
					logger.Trace("Stop processing backlog", "msg", m)
					backlog.Push(m, prio)
					isFuture = true
					break
				}
				logger.Trace("Skip the backlog event", "msg", m, "err", err)
				continue
			}
			logger.Trace("Post backlog event", "msg", m)

			event.src = src
			go c.sendEvent(event)
		}
	}
}

func toPriority(msgCode uint64, view *View) float32 {
	if msgCode == msgRoundChange {
		// For msgRoundChange, set the message priority based on its sequence
		return -float32(view.Sequence.Uint64() * 1000)
	}
	// FIXME: round will be reset as 0 while new sequence
	// 10 * Round limits the range of message code is from 0 to 9
	// 1000 * Sequence limits the range of round is from 0 to 99
	return -float32(view.Sequence.Uint64()*1000 + view.Round.Uint64()*10 + uint64(msgPriority[msgCode]))
}
