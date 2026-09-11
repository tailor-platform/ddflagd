package bridge

import (
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
)

// startTracer starts the tracer with no options on purpose. The service
// identifiers have to reach the Remote Configuration client, the exposure
// payload and the evaluation metrics, and only the environment is read by all
// three: the OTel metric resource behind feature_flag.evaluations reads
// DD_SERVICE and friends and never falls back to the tracer's options.
func startTracer() error {
	return tracer.Start()
}

// StopTracer stops the tracer. It is the last step of the shutdown sequence.
func StopTracer() {
	tracer.Stop()
}
