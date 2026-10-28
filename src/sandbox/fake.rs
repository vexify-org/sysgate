//! In-memory sandbox driver for tests and local demos.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use chrono::Utc;

use super::{Error, ExecRequest, ExecResult, Handle, Result, Spec};

struct State {
    sandboxes: HashMap<String, Handle>,
}

pub struct FakeDriver {
    state: Mutex<State>,
    seq: AtomicU64,
}

impl Default for FakeDriver {
    fn default() -> Self {
        Self::new()
    }
}

impl FakeDriver {
    pub fn new() -> Self {
        Self {
            state: Mutex::new(State {
                sandboxes: HashMap::new(),
            }),
            seq: AtomicU64::new(0),
        }
    }

    pub async fn create(&self, spec: &Spec) -> Result<Handle> {
        let mut state = self.state.lock().expect("fake driver lock");
        let id = format!("fake-{}", self.seq.fetch_add(1, Ordering::Relaxed) + 1);
        let now = Utc::now();
        let handle = Handle {
            id: id.clone(),
            driver: "fake".into(),
            created_at: now,
            expires_at: spec
                .ttl
                .and_then(|ttl| chrono::Duration::from_std(ttl).ok())
                .map(|d| now + d),
            labels: None,
        };
        state.sandboxes.insert(id, handle.clone());
        Ok(handle)
    }

    pub async fn exec(&self, id: &str, req: &ExecRequest) -> Result<ExecResult> {
        let state = self.state.lock().expect("fake driver lock");
        if !state.sandboxes.contains_key(id) {
            return Err(Error::NotFound);
        }
        Ok(ExecResult {
            stdout: format!("fake:{}", req.argv.join(" ")).into_bytes(),
            stderr: Vec::new(),
            exit_code: 0,
        })
    }

    pub async fn destroy(&self, id: &str) -> Result<()> {
        let mut state = self.state.lock().expect("fake driver lock");
        if state.sandboxes.remove(id).is_none() {
            return Err(Error::NotFound);
        }
        Ok(())
    }
}
