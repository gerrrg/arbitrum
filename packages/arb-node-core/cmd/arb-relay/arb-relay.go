/*
 * Copyright 2021, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"flag"
	"github.com/pkg/errors"
	golog "log"
	"net/http"
	"os"
	"time"

	"github.com/offchainlabs/arbitrum/packages/arb-node-core/cmdhelp"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcastclient"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcaster"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/rs/zerolog/pkgerrors"
)

var logger zerolog.Logger
var pprofMux *http.ServeMux

type ArbRelay struct {
	ArbitrumBroadcasterWebsocketUrl string
	broadcastClient                 *broadcastclient.BroadcastClient
	broadcaster                     *broadcaster.Broadcaster
}

func init() {
	pprofMux = http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
}

func main() {
	// Enable line numbers in logging
	golog.SetFlags(golog.LstdFlags | golog.Lshortfile)

	// Print stack trace when `.Error().Stack().Err(err).` is added to zerolog call
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Print line number that log was created on
	logger = log.With().Caller().Stack().Str("component", "arb-validator").Logger()

	if err := startup(); err != nil {
		logger.Error().Err(err).Msg("Error running validator")
	}
}

func startup() error {
	defer logger.Log().Msg("Cleanly shutting down relay")
	ctx, cancelFunc, cancelChan := cmdhelp.CreateLaunchContext()
	defer cancelFunc()

	flagSet := flag.NewFlagSet("arb-relay", flag.ExitOnError)
	enablePProf := flagSet.Bool("pprof", false, "enable profiling server")
	gethLogLevel, arbLogLevel := cmdhelp.AddLogFlags(flagSet)

	// Relay Config
	enableDebug := flagSet.Bool("debug", false, "Enable debug logging")
	sequencerWebsocketURL := flagSet.String("sequencer-websocket-url", "", "websocket address of sequencer feed source")
	relayWebsocketURL := flagSet.String("relay-websocket-url", "0.0.0.0:9742", "address to bind the sequencer feed output to")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return errors.Wrap(err, "failed parsing command line arguments")
	}
	if err := cmdhelp.ParseLogFlags(gethLogLevel, arbLogLevel); err != nil {
		return err
	}

	if *sequencerWebsocketURL == "" {
		return errors.New("Missing --relayWebsocketURL")
	}

	if *enablePProf {
		go func() {
			err := http.ListenAndServe("localhost:8081", pprofMux)
			log.Error().Err(err).Msg("profiling server failed")
		}()
	}

	broadcasterSettings := broadcaster.Settings{
		Addr:      *relayWebsocketURL,
		Workers:   128,
		Queue:     1,
		IoTimeout: 2 * time.Second,
	}

	bc := broadcaster.NewBroadcaster(broadcasterSettings)

	err := bc.Start(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("unable to start broadcaster")
	}
	defer bc.Stop()

	relaySettings := broadcaster.Settings{
		Addr:      *sequencerWebsocketURL,
		Workers:   128,
		Queue:     1,
		IoTimeout: 2 * time.Second,
	}

	// Start up an arbitrum sequencer relay
	arbRelay := NewArbRelay(*sequencerWebsocketURL, relaySettings)
	relayDone := arbRelay.Start(ctx, *enableDebug)
	defer arbRelay.Stop()

	select {
	case <-cancelChan:
		return nil
	case <-relayDone:
		return nil
	}
}

func NewArbRelay(websocketUrl string, rebroadcastSettings broadcaster.Settings) *ArbRelay {
	ar := &ArbRelay{}
	ar.ArbitrumBroadcasterWebsocketUrl = websocketUrl

	ar.broadcaster = broadcaster.NewBroadcaster(rebroadcastSettings)

	return ar
}

func (ar *ArbRelay) Start(ctx context.Context, debug bool) chan bool {
	done := make(chan bool)
	ar.broadcastClient = broadcastclient.NewBroadcastClient(ar.ArbitrumBroadcasterWebsocketUrl, nil)

	err := ar.broadcaster.Start(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("broadcasted unable to start")
	}

	// connect returns
	messages, err := ar.broadcastClient.Connect()
	if err != nil {
		logger.Error().Err(err).Msg("broadcast client unable to connect")
	}

	go func() {
		defer func() {
			done <- true
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-messages:
				if debug {
					logger.Info().Hex("acc", msg.FeedItem.BatchItem.Accumulator.Bytes()).Msg("batch sent")
				}
				err = ar.broadcaster.Broadcast(msg.FeedItem.PrevAcc, msg.FeedItem.BatchItem, msg.Signature)
				if err != nil {
					logger.
						Error().
						Err(err).
						Hex("PrevAcc", msg.FeedItem.PrevAcc.Bytes()).
						Hex("BatchItem", msg.FeedItem.BatchItem.ToBytesWithSeqNum()).
						Msg("unable to broadcast batch item")
				}
			case ca := <-ar.broadcastClient.ConfirmedAccumulatorListener:
				if debug {
					logger.Info().Hex("acc", ca.Bytes()).Msg("confirmed accumulator")
				}
				err = ar.broadcaster.ConfirmedAccumulator(ca)
				if err != nil {
					logger.
						Error().
						Err(err).
						Hex("acc", ca.Bytes()).
						Msg("unable to broadcast confirmed accumulator")
				}
			}
		}
	}()

	return done
}

func (ar *ArbRelay) Stop() {
	ar.broadcastClient.Close()
	ar.broadcaster.Stop()
}