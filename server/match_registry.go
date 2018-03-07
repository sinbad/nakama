// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"database/sql"
	"github.com/heroiclabs/nakama/social"
	"github.com/satori/go.uuid"
	"github.com/yuin/gopher-lua"
	"go.uber.org/zap"
	"sync"
	"time"
)

type MatchPresence struct {
	Node      string
	UserId    uuid.UUID
	SessionId uuid.UUID
	Username  string
}

type MatchRegistry interface {
	// Create and start a new match, given a Lua module name.
	NewMatch(name string) (*MatchHandler, error)
	// Remove a tracked match and ensure all its presences are cleaned up.
	// Does not ensure the match process itself is no longer running, that must be handled separately.
	RemoveMatch(id uuid.UUID, stream PresenceStream)
	// Stop the match registry and close all matches it's tracking.
	Stop()

	// Pass a user join attempt to a match handler. Returns if the match was found, and if the join was accepted.
	Join(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string) (bool, bool)
	// Notify a match handler that a user has left or disconnected.
	Leave(id uuid.UUID, node string, presences []Presence)
	// Called by match handlers to request the removal fo a match participant.
	Kick(stream PresenceStream, presences []*MatchPresence)
	// Pass a data payload (usually from a user) to the appropriate match handler.
	// Assumes that the data sender has already been validated as a match participant before this call.
	SendData(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string, opCode int64, data []byte)
}

type LocalMatchRegistry struct {
	sync.RWMutex
	logger          *zap.Logger
	db              *sql.DB
	config          Config
	socialClient    *social.Client
	sessionRegistry *SessionRegistry
	tracker         Tracker
	router          MessageRouter
	stdLibs         map[string]lua.LGFunction
	modules         *sync.Map
	once            *sync.Once
	node            string
	matches         map[uuid.UUID]*MatchHandler
}

func NewLocalMatchRegistry(logger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, sessionRegistry *SessionRegistry, tracker Tracker, router MessageRouter, stdLibs map[string]lua.LGFunction, once *sync.Once, node string) MatchRegistry {
	return &LocalMatchRegistry{
		logger:          logger,
		db:              db,
		config:          config,
		socialClient:    socialClient,
		sessionRegistry: sessionRegistry,
		tracker:         tracker,
		router:          router,
		stdLibs:         stdLibs,
		once:            once,
		node:            node,
		matches:         make(map[uuid.UUID]*MatchHandler),
	}
}

func (r *LocalMatchRegistry) NewMatch(name string) (*MatchHandler, error) {
	id := uuid.NewV4()
	match, err := NewMatchHandler(r.logger, r.db, r.config, r.socialClient, r.sessionRegistry, r, r.tracker, r.router, r.stdLibs, r.once, id, r.node, name)
	if err != nil {
		return nil, err
	}
	r.Lock()
	r.matches[id] = match
	r.Unlock()
	return match, nil
}

func (r *LocalMatchRegistry) RemoveMatch(id uuid.UUID, stream PresenceStream) {
	r.Lock()
	delete(r.matches, id)
	r.Unlock()
	r.tracker.UntrackByStream(stream)
}

func (r *LocalMatchRegistry) Stop() {
	r.Lock()
	for id, mh := range r.matches {
		mh.Close()
		delete(r.matches, id)
	}
	r.Unlock()
}

func (r *LocalMatchRegistry) Join(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string) (bool, bool) {
	if node != r.node {
		return false, false
	}

	var mh *MatchHandler
	var ok bool
	r.RLock()
	mh, ok = r.matches[id]
	r.RUnlock()
	if !ok {
		return false, false
	}

	resultCh := make(chan bool, 1)
	if !mh.QueueCall(JoinAttempt(resultCh, userID, sessionID, username, fromNode)) {
		// The match call queue was full, so will be closed and therefore can't be joined.
		return true, false
	}

	// Set up a limit to how long the call will wait, default is 10 seconds.
	ticker := time.NewTicker(time.Second * 10)
	select {
	case <-ticker.C:
		ticker.Stop()
		// The join attempt has timed out, join is assumed to be rejected.
		return true, false
	case r := <-resultCh:
		ticker.Stop()
		// The join attempt has returned a result.
		return true, r
	}
}

func (r *LocalMatchRegistry) Leave(id uuid.UUID, node string, presences []Presence) {
	if node != r.node {
		return
	}

	var mh *MatchHandler
	var ok bool
	r.RLock()
	mh, ok = r.matches[id]
	r.RUnlock()
	if !ok {
		return
	}

	// Doesn't matter if the call queue was full. If the match is being closed then leaves don't matter anyway.
	mh.QueueCall(Leave(presences))
}

func (r *LocalMatchRegistry) Kick(stream PresenceStream, presences []*MatchPresence) {
	for _, presence := range presences {
		if presence.Node != r.node {
			continue
		}
		r.tracker.Untrack(presence.SessionId, stream, presence.UserId)
	}
}

func (r *LocalMatchRegistry) SendData(id uuid.UUID, node string, userID, sessionID uuid.UUID, username, fromNode string, opCode int64, data []byte) {
	if node != r.node {
		return
	}

	var mh *MatchHandler
	var ok bool
	r.RLock()
	mh, ok = r.matches[id]
	r.RUnlock()
	if !ok {
		return
	}

	mh.QueueData(&MatchDataMessage{
		UserID:    userID,
		SessionID: sessionID,
		Username:  username,
		Node:      node,
		OpCode:    opCode,
		Data:      data,
	})
}
