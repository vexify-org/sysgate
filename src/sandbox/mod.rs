//! Driver abstraction for isolated workloads: gVisor via runsc, in-memory
//! fakes in tests.

pub mod fake;
pub mod runsc;

use std::collections::HashMap;
use std::time::Duration;

use chrono::{DateTime, Utc};

#[derive(Debug)]
pub enum Error {
    NotFound,
    Invalid(String),
    Io(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::NotFound => write!(f, "sandbox not found"),
            Error::Invalid(msg) | Error::Io(msg) => write!(f, "{msg}"),
        }
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

/// Describes a sandbox to create.
#[derive(Debug, Clone, Default)]
pub struct Spec {
    pub rootfs: Option<String>,
    pub init: Vec<String>,
    pub env: Vec<String>,
    pub ttl: Option<Duration>,
}

/// A created sandbox.
#[derive(Debug, Clone, serde::Serialize)]
pub struct Handle {
    pub id: String,
    pub driver: String,
    pub created_at: DateTime<Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<DateTime<Utc>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub labels: Option<HashMap<String, String>>,
}

/// A command to run inside a sandbox.
#[derive(Debug, Clone)]
pub struct ExecRequest {
    pub argv: Vec<String>,
    pub env: Vec<String>,
    pub workdir: Option<String>,
}

/// The outcome of an [`ExecRequest`].
#[derive(Debug, Clone)]
pub struct ExecResult {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub exit_code: i32,
}

pub enum Driver {
    Fake(fake::FakeDriver),
    Runsc(runsc::RunscDriver),
}

impl Driver {
    pub fn name(&self) -> &'static str {
        match self {
            Driver::Fake(_) => "fake",
            Driver::Runsc(_) => "runsc",
        }
    }

    pub async fn create(&self, spec: &Spec) -> Result<Handle> {
        match self {
            Driver::Fake(d) => d.create(spec).await,
            Driver::Runsc(d) => d.create(spec).await,
        }
    }

    pub async fn exec(&self, id: &str, req: &ExecRequest) -> Result<ExecResult> {
        match self {
            Driver::Fake(d) => d.exec(id, req).await,
            Driver::Runsc(d) => d.exec(id, req).await,
        }
    }

    pub async fn destroy(&self, id: &str) -> Result<()> {
        match self {
            Driver::Fake(d) => d.destroy(id).await,
            Driver::Runsc(d) => d.destroy(id).await,
        }
    }
}
