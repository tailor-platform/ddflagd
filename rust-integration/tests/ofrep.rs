//! What the official OpenFeature Rust crates do against a ddflagd OFREP
//! server.
//!
//! ddflagd ships no Rust code, so these tests are about the official crates,
//! not about this project's own code. They state the behavior a consuming
//! application can rely on, including the rough edges of
//! `open-feature-ofrep` 0.1: every reason comes back as STATIC, the flag
//! metadata is dropped, and a code default is indistinguishable from a
//! transport failure. Each of those is a deliberate trade-off in ddflagd's
//! design, and pinning it here means a crate update that changes it is noticed
//! here rather than in production.

use std::env;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::OnceLock;
use std::time::Duration;

use open_feature::provider::FeatureProvider;
use open_feature::{
    EvaluationContext, EvaluationContextFieldValue, EvaluationErrorCode, EvaluationReason, Value,
};
use open_feature_ofrep::{OfrepOptions, OfrepProvider};

/// Stub is the ddflagd OFREP server the tests talk to.
struct Stub {
    child: Child,
    base_url: String,
}

/// stub_binary returns the stub server binary, building it once per test run.
///
/// CI passes a prebuilt binary through DDFLAGD_OFREP_STUB; the fallback keeps
/// `cargo test` working on its own without a separate build step.
fn stub_binary() -> PathBuf {
    static BINARY: OnceLock<PathBuf> = OnceLock::new();
    BINARY
        .get_or_init(|| {
            if let Ok(path) = env::var("DDFLAGD_OFREP_STUB") {
                return PathBuf::from(path);
            }
            let manifest = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
            let binary = manifest.join("target").join("ofrepstub");
            let status = Command::new("go")
                .args(["build", "-o", binary.to_str().unwrap(), "./ofrepstub"])
                .current_dir(&manifest)
                .status()
                .expect("building the ddflagd OFREP stub");
            assert!(status.success(), "building the ddflagd OFREP stub failed");
            binary
        })
        .clone()
}

impl Stub {
    /// Starts the stub server and waits for it to print its address.
    fn start() -> Stub {
        let mut child = Command::new(stub_binary())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("starting the ddflagd OFREP stub");

        let stdout = child.stdout.take().expect("the stub has no stdout");
        let base_url = read_first_line(stdout);

        Stub { child, base_url }
    }

    async fn provider(&self) -> OfrepProvider {
        OfrepProvider::new(OfrepOptions {
            base_url: self.base_url.clone(),
            connect_timeout: Duration::from_millis(500),
            ..Default::default()
        })
        .await
        .expect("building the official OFREP provider")
    }
}

impl Drop for Stub {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn read_first_line(stdout: std::process::ChildStdout) -> String {
    use std::io::{BufRead, BufReader};

    let mut reader = BufReader::new(stdout);
    let mut line = String::new();
    reader
        .read_line(&mut line)
        .expect("reading the stub's address");
    let line = line.trim().to_string();
    assert!(
        line.starts_with("http://"),
        "the stub printed {line:?} instead of its address"
    );
    // The reader is dropped here, which closes the pipe. The stub only writes
    // one line, so nothing is lost.
    line
}

fn context() -> EvaluationContext {
    EvaluationContext::default()
        .with_targeting_key("alice")
        .with_custom_field("country", "France")
}

/// A flag with a value resolves to that value for every primitive type.
#[tokio::test]
async fn values_resolve_for_every_primitive_type() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    let boolean = provider
        .resolve_bool_value("value-bool", &ctx)
        .await
        .expect("resolving a boolean");
    assert!(boolean.value);
    assert_eq!(boolean.variant.as_deref(), Some("on"));

    // A false value has to survive the round trip: ddflagd omits the value
    // only for a code default, never because the value is falsy.
    let falsy = provider
        .resolve_bool_value("value-false", &ctx)
        .await
        .expect("resolving a false boolean");
    assert!(!falsy.value);

    let string = provider
        .resolve_string_value("value-string", &ctx)
        .await
        .expect("resolving a string");
    assert_eq!(string.value, "green");

    let integer = provider
        .resolve_int_value("value-int", &ctx)
        .await
        .expect("resolving an integer");
    assert_eq!(integer.value, 42);

    let float = provider
        .resolve_float_value("value-float", &ctx)
        .await
        .expect("resolving a float");
    assert!((float.value - 3.5).abs() < f64::EPSILON);

    let structure = provider
        .resolve_struct_value("value-object", &ctx)
        .await
        .expect("resolving an object");
    assert_eq!(
        structure.value.fields.get("string"),
        Some(&Value::String("one".to_string()))
    );
}

/// The crate reports every reason as STATIC and drops the flag metadata.
///
/// ddflagd accepts this: an application needs the flag's value, and the
/// telemetry that would use the allocation key is emitted by the official Go
/// SDK inside the bridge, not by the caller.
#[tokio::test]
async fn the_reason_and_the_flag_metadata_are_not_carried_through() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    // The server answers TARGETING_MATCH with four metadata entries.
    let details = provider
        .resolve_bool_value("value-bool", &ctx)
        .await
        .expect("resolving a boolean");

    assert!(
        matches!(details.reason, Some(EvaluationReason::Static)),
        "open-feature-ofrep 0.1 reports every reason as STATIC, got {:?}",
        details.reason
    );
    assert!(
        details.flag_metadata.is_none()
            || details
                .flag_metadata
                .as_ref()
                .is_some_and(|m| m.values.is_empty()),
        "open-feature-ofrep 0.1 drops the flag metadata, got {:?}",
        details.flag_metadata
    );
}

/// A code default and a disabled flag both come back as an error, which is how
/// an application falls back to the value in its own code.
#[tokio::test]
async fn a_code_default_is_an_error_the_caller_falls_back_from() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    for flag in ["code-default", "disabled"] {
        let err = provider
            .resolve_bool_value(flag, &ctx)
            .await
            .expect_err("a valueless response must not resolve to a value");
        assert_eq!(
            err.code,
            EvaluationErrorCode::ParseError,
            "{flag}: open-feature-ofrep 0.1 reports a missing value as a parse error"
        );

        // This is the trade-off ddflagd accepts: the caller cannot tell a code
        // default from a failure, so the share of evaluations that fell back is
        // monitored on the bridge side instead.
        let value = provider.resolve_bool_value(flag, &ctx).await.ok();
        assert!(value.is_none(), "{flag} must not carry a value");
    }
}

/// Every failure ddflagd can answer with maps onto an error code the crate
/// produces, and never onto a value.
#[tokio::test]
async fn failures_map_onto_error_codes() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    let cases: Vec<(&str, EvaluationErrorCode)> = vec![
        // 404
        ("flag-that-does-not-exist", EvaluationErrorCode::FlagNotFound),
        // 400, all of which the crate collapses into InvalidContext
        (
            "targeting-key-missing",
            EvaluationErrorCode::InvalidContext,
        ),
        ("parse-error", EvaluationErrorCode::InvalidContext),
        ("invalid-context", EvaluationErrorCode::InvalidContext),
        // 503, which the crate cannot tell from a malformed body
        ("provider-not-ready", EvaluationErrorCode::ParseError),
        // 500
        ("general-error", EvaluationErrorCode::ParseError),
    ];

    for (flag, want) in cases {
        let err = provider
            .resolve_bool_value(flag, &ctx)
            .await
            .expect_err("a failure must not resolve to a value");
        assert_eq!(err.code, want, "flag {flag}");
    }
}

/// A type that does not match the flag's value is an error on the Rust side,
/// because the bridge evaluates without knowing the type.
#[tokio::test]
async fn a_mismatched_type_is_an_error_on_the_caller_side() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    let err = provider
        .resolve_bool_value("value-string", &ctx)
        .await
        .expect_err("a string value must not resolve as a boolean");
    assert_eq!(err.code, EvaluationErrorCode::ParseError);
}

/// The bridge bounds the response time, because the crate has no overall
/// request timeout of its own: only the connect timeout is configurable.
#[tokio::test]
async fn the_bridge_bounds_a_stalled_evaluation() {
    let stub = Stub::start();
    let provider = stub.provider().await;
    let ctx = context();

    let started = std::time::Instant::now();
    let err = provider
        .resolve_bool_value("slow", &ctx)
        .await
        .expect_err("a timed out evaluation must not resolve to a value");

    assert_eq!(err.code, EvaluationErrorCode::ParseError);
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "the bridge did not bound the evaluation: it took {:?}",
        started.elapsed()
    );
}

/// A nested context field is sent as a debug formatted string rather than as
/// JSON.
///
/// The bridge rejects a JSON object in the evaluation context, but it never
/// sees one: the crate stringifies the field first, and the value that reaches
/// Datadog is the debug form of an opaque Any. Passing flat primitives only is
/// therefore the caller's responsibility, which no amount of validation in the
/// bridge can take over.
#[tokio::test]
async fn a_nested_context_field_is_sent_as_a_string() {
    let stub = Stub::start();
    let provider = stub.provider().await;

    let ctx = EvaluationContext::default()
        .with_targeting_key("alice")
        .with_custom_field(
            "user",
            EvaluationContextFieldValue::new_struct(("id", "1")),
        );

    // The bridge rejects a JSON object in the context with 400, which the crate
    // reports as InvalidContext. A nested field never reaches that path because
    // the crate stringifies it first, so this resolves normally instead.
    let details = provider
        .resolve_bool_value("value-bool", &ctx)
        .await
        .expect("a stringified nested field is accepted as a flat attribute");
    assert!(details.value);
}

/// The evaluation goes through an OpenFeature client the same way an
/// application uses it, so that the shape the README recommends is covered.
#[tokio::test]
async fn a_client_evaluation_falls_back_to_the_code_default() {
    use open_feature::OpenFeature;

    let stub = Stub::start();
    let provider = stub.provider().await;

    // The API is a process wide singleton, so this test owns it for its
    // duration. The other tests use the provider directly to stay independent.
    let mut api = OpenFeature::singleton_mut().await;
    api.set_provider(provider).await;
    drop(api);

    let client = OpenFeature::singleton().await.create_client();
    let ctx = context();

    let enabled = tokio::time::timeout(
        Duration::from_millis(250),
        client.get_bool_value("value-bool", Some(&ctx), None),
    )
    .await
    .ok()
    .and_then(Result::ok)
    .unwrap_or(false);
    assert!(enabled, "a flag with a value must resolve through a client");

    let fallback = tokio::time::timeout(
        Duration::from_millis(250),
        client.get_bool_value("code-default", Some(&ctx), None),
    )
    .await
    .ok()
    .and_then(Result::ok)
    .unwrap_or(false);
    assert!(
        !fallback,
        "a code default must leave the caller's own default in place"
    );
}
