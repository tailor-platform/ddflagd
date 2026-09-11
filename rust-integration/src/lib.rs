//! ddflagd provides no Rust code: a consuming application uses the official
//! `open-feature` and `open-feature-ofrep` crates directly.
//!
//! This crate holds the integration tests in `tests/`, which pin what those
//! crates do against a ddflagd OFREP server so that a crate update that changes
//! their behavior is noticed here rather than in production.
