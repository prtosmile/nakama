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
	"context"
	"database/sql"
	"errors"
	"github.com/heroiclabs/nakama/rtapi"
	"github.com/heroiclabs/nakama/runtime"
	"github.com/heroiclabs/nakama/social"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"log"
	"path/filepath"
	"plugin"
	"strings"
)

type RuntimeProviderGo struct {
	// TODO
}

type RuntimeGoInitialiser struct {
	logger *log.Logger
	db     *sql.DB
	env    map[string]string
	nk     runtime.NakamaModule

	rpc               map[string]Runtime2RpcFunction
	beforeRt          map[string]Runtime2BeforeRtFunction
	afterRt           map[string]Runtime2AfterRtFunction
	matchmakerMatched Runtime2MatchmakerMatchedFunction
}

func (ri *RuntimeGoInitialiser) RegisterRpc(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error, int)) error {
	id = strings.ToLower(id)
	ri.rpc[id] = func(queryParams map[string][]string, userID, username string, expiry int64, sessionID, clientIP, clientPort, payload string) (string, error, codes.Code) {
		ctx := NewRuntimeGoContext(ri.env, RuntimeExecutionModeRPC, queryParams, expiry, userID, username, sessionID, clientIP, clientPort)
		result, fnErr, code := fn(ctx, ri.logger, ri.db, ri.nk, payload)
		return result, fnErr, codes.Code(code)
	}
	return nil
}

func (ri *RuntimeGoInitialiser) RegisterBeforeRt(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, envelope *rtapi.Envelope) (*rtapi.Envelope, error)) error {
	id = strings.ToLower(RTAPI_PREFIX + id)
	ri.beforeRt[id] = func(logger *zap.Logger, userID, username string, expiry int64, sessionID, clientIP, clientPort string, envelope *rtapi.Envelope) (*rtapi.Envelope, error) {
		ctx := NewRuntimeGoContext(ri.env, RuntimeExecutionModeBefore, nil, expiry, userID, username, sessionID, clientIP, clientPort)
		return fn(ctx, ri.logger, ri.db, ri.nk, envelope)
	}
	return nil
}

func (ri *RuntimeGoInitialiser) RegisterAfterRt(id string, fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, envelope *rtapi.Envelope) error) error {
	id = strings.ToLower(RTAPI_PREFIX + id)
	ri.afterRt[id] = func(logger *zap.Logger, userID, username string, expiry int64, sessionID, clientIP, clientPort string, envelope *rtapi.Envelope) error {
		ctx := NewRuntimeGoContext(ri.env, RuntimeExecutionModeAfter, nil, expiry, userID, username, sessionID, clientIP, clientPort)
		return fn(ctx, ri.logger, ri.db, ri.nk, envelope)
	}
	return nil
}

func (ri *RuntimeGoInitialiser) RegisterMatchmakerMatched(fn func(ctx context.Context, logger *log.Logger, db *sql.DB, nk runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error)) error {
	ri.matchmakerMatched = func(entries []*MatchmakerEntry) (string, error) {
		ctx := NewRuntimeGoContext(ri.env, RuntimeExecutionModeMatchmaker, nil, 0, "", "", "", "", "")
		runtimeEntries := make([]runtime.MatchmakerEntry, len(entries))
		for i, entry := range entries {
			runtimeEntries[i] = runtime.MatchmakerEntry(entry)
		}
		return fn(ctx, ri.logger, ri.db, ri.nk, runtimeEntries)
	}
	return nil
}

func NewRuntimeProviderGo(logger, startupLogger *zap.Logger, db *sql.DB, config Config, socialClient *social.Client, leaderboardCache LeaderboardCache, sessionRegistry *SessionRegistry, matchRegistry MatchRegistry, tracker Tracker, router MessageRouter, rootPath string, paths []string) ([]string, map[string]Runtime2RpcFunction, map[string]Runtime2BeforeRtFunction, map[string]Runtime2AfterRtFunction, Runtime2MatchmakerMatchedFunction, RuntimeProvider, error) {
	modulePaths := make([]string, 0)

	initialiser := &RuntimeGoInitialiser{
		logger:   zap.NewStdLog(logger),
		db:       db,
		env:      config.GetRuntime().Environment,
		nk:       NewRuntimeGoNakamaModule(logger, db, config, socialClient, leaderboardCache, sessionRegistry, matchRegistry, tracker, router),
		rpc:      make(map[string]Runtime2RpcFunction, 0),
		beforeRt: make(map[string]Runtime2BeforeRtFunction, 0),
		afterRt:  make(map[string]Runtime2AfterRtFunction, 0),
	}

	for _, path := range paths {
		if strings.ToLower(filepath.Ext(path)) != ".so" {
			continue
		}

		relPath, _ := filepath.Rel(rootPath, path)
		name := strings.TrimSuffix(relPath, filepath.Ext(relPath))

		// Open the plugin.
		p, err := plugin.Open(path)
		if err != nil {
			startupLogger.Error("Could not open Go module", zap.String("path", path), zap.Error(err))
			return nil, nil, nil, nil, nil, nil, err
		}

		// Look up the required initialisation function.
		f, err := p.Lookup("InitModule")
		if err != nil {
			startupLogger.Fatal("Error looking up InitModule function in Go module", zap.String("name", name))
			return nil, nil, nil, nil, nil, nil, err
		}

		// Ensure the function has the correct signature.
		fn, ok := f.(func(context.Context, *log.Logger, *sql.DB, runtime.NakamaModule, runtime.Initialiser))
		if !ok {
			startupLogger.Fatal("Error reading InitModule function in Go module", zap.String("name", name))
			return nil, nil, nil, nil, nil, nil, errors.New("error reading InitModule function in Go module")
		}

		// Run the initialisation.
		fn(context.Background(), initialiser.logger, db, initialiser.nk, initialiser)
		modulePaths = append(modulePaths, relPath)
	}

	runtimeProviderGo := &RuntimeProviderGo{}

	return modulePaths, initialiser.rpc, initialiser.beforeRt, initialiser.afterRt, initialiser.matchmakerMatched, runtimeProviderGo, nil
}