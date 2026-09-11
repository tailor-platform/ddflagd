// Command ofrepstub serves ddflagd's OFREP handler over a scripted provider.
//
// It exists so that the Rust integration tests can drive the real request
// conversion and response mapping of internal/ofrep without needing a Datadog
// Agent. Each flag key names one row of the mapping table, which lets the Rust
// tests state what the official open-feature-ofrep crate does with each of
// them.
//
// It prints the listen address on the first line of stdout and then serves
// until it is terminated.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/isolated"

	"github.com/k1LoW/ddflagd/internal/bridge"
	"github.com/k1LoW/ddflagd/internal/ofrep"
)

// handlerTimeout is short so that the slow flag below reaches it quickly.
const handlerTimeout = 100 * time.Millisecond

// slowFlag blocks past the handler timeout.
const slowFlag = "slow"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	provider := &scriptedProvider{results: results()}

	api := isolated.NewAPI()
	if err := api.SetProviderAndWait(context.Background(), provider); err != nil {
		return fmt.Errorf("setting the provider: %w", err)
	}

	handler, err := ofrep.NewHandler(ofrep.Options{
		Evaluator: &clientEvaluator{client: api.NewClient()},
		Timeout:   handlerTimeout,
	})
	if err != nil {
		return err
	}

	addr := os.Getenv("OFREP_STUB_ADDR")
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Go's os.Stdout is unbuffered, so the address is on the pipe as soon as
	// this returns, which is what the Rust tests wait for.
	fmt.Printf("http://%s\n", listener.Addr().String())

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(listener)
}

// results maps one flag key to each row of the mapping table.
func results() map[string]openfeature.InterfaceResolutionDetail {
	metadata := openfeature.FlagMetadata{
		"dd.allocation.key":    "allocation-1",
		"dd.doLog":             true,
		"dd.serialId":          int64(7),
		"dd.eval.timestamp_ms": int64(1757500000000),
	}
	value := func(v any, reason openfeature.Reason, variant string) openfeature.InterfaceResolutionDetail {
		return openfeature.InterfaceResolutionDetail{
			Value: v,
			ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
				Reason:       reason,
				Variant:      variant,
				FlagMetadata: metadata,
			},
		}
	}
	failure := func(err openfeature.ResolutionError) openfeature.InterfaceResolutionDetail {
		return openfeature.InterfaceResolutionDetail{
			ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
				ResolutionError: err,
				Reason:          openfeature.ErrorReason,
			},
		}
	}

	return map[string]openfeature.InterfaceResolutionDetail{
		"value-bool":   value(true, openfeature.TargetingMatchReason, "on"),
		"value-false":  value(false, openfeature.TargetingMatchReason, "off"),
		"value-string": value("green", openfeature.StaticReason, "green"),
		"value-int":    value(int64(42), openfeature.StaticReason, "answer"),
		"value-float":  value(3.5, openfeature.SplitReason, "half"),
		"value-object": value(map[string]any{"integer": 1, "string": "one"}, openfeature.SplitReason, "one"),

		"code-default": {ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
			Reason:       openfeature.DefaultReason,
			FlagMetadata: metadata,
		}},
		"disabled": {ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
			Reason: openfeature.DisabledReason,
		}},

		"targeting-key-missing": failure(openfeature.NewTargetingKeyMissingResolutionError("targeting key missing")),
		"parse-error":           failure(openfeature.NewParseErrorResolutionError("parse error")),
		"invalid-context":       failure(openfeature.NewInvalidContextResolutionError("invalid context")),
		"provider-not-ready":    failure(openfeature.NewProviderNotReadyResolutionError("no configuration loaded")),
		"general-error":         failure(openfeature.NewGeneralResolutionError("boom")),

		slowFlag: value(true, openfeature.StaticReason, "on"),
	}
}

// clientEvaluator evaluates through an OpenFeature client, the same way the
// bridge does.
type clientEvaluator struct {
	client *openfeature.Client
}

func (e *clientEvaluator) Evaluate(ctx context.Context, key string, evalCtx openfeature.EvaluationContext) (openfeature.InterfaceEvaluationDetails, error) {
	return e.client.ObjectValueDetails(ctx, key, nil, evalCtx)
}

func (e *clientEvaluator) State() bridge.State { return bridge.StateReady }

// scriptedProvider answers with the prepared resolution for a flag key, and
// reports an unknown key the way the official provider does.
type scriptedProvider struct {
	openfeature.NoopProvider
	results map[string]openfeature.InterfaceResolutionDetail
}

func (p *scriptedProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "ofrepstub"}
}

func (p *scriptedProvider) ObjectEvaluation(ctx context.Context, flag string, _ any, _ openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	if flag == slowFlag {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
	}
	res, ok := p.results[flag]
	if !ok {
		return openfeature.InterfaceResolutionDetail{
			ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
				ResolutionError: openfeature.NewFlagNotFoundResolutionError("flag not found"),
				Reason:          openfeature.ErrorReason,
			},
		}
	}
	return res
}
