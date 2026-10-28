//! HTTP API: sandbox lifecycle, exec, health and Prometheus metrics.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::body::Bytes;
use axum::extract::{Path, State};
use axum::http::{header, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use chrono::Utc;
use serde::Deserialize;
use serde_json::json;

use crate::pool::Pool;
use crate::sandbox::{Driver, Error as SandboxError, ExecRequest, Handle};

#[derive(Debug, Clone)]
pub struct Config {
    pub default_ttl: Duration,
    pub max_sandboxes: usize,
    pub exec_timeout: Duration,
    pub reap_interval: Duration,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            default_ttl: Duration::from_secs(600),
            max_sandboxes: 256,
            exec_timeout: Duration::from_secs(30),
            reap_interval: Duration::from_secs(1),
        }
    }
}

pub struct AppState {
    pub driver: Arc<Driver>,
    pub pool: Arc<Pool>,
    pub cfg: Config,
    pub active: tokio::sync::RwLock<HashMap<String, Handle>>,
    pub started: Instant,
    pub created: AtomicU64,
    pub destroyed: AtomicU64,
    pub execs: AtomicU64,
    pub errors: AtomicU64,
}

impl AppState {
    pub fn new(driver: Arc<Driver>, pool: Arc<Pool>, cfg: Config) -> Arc<Self> {
        Arc::new(Self {
            driver,
            pool,
            cfg,
            active: tokio::sync::RwLock::new(HashMap::new()),
            started: Instant::now(),
            created: AtomicU64::new(0),
            destroyed: AtomicU64::new(0),
            execs: AtomicU64::new(0),
            errors: AtomicU64::new(0),
        })
    }
}

pub fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(health))
        .route("/metrics", get(metrics))
        .route("/v1/sandboxes", post(create).get(list))
        .route("/v1/sandboxes/{id}", get(get_one).delete(delete_one))
        .route("/v1/sandboxes/{id}/exec", post(exec))
        .with_state(state)
}

#[derive(Deserialize, Default)]
struct CreateRequest {
    #[serde(default)]
    ttl_seconds: f64,
    #[serde(default)]
    labels: Option<HashMap<String, String>>,
}

#[derive(Deserialize)]
struct ExecBody {
    argv: Vec<String>,
    #[serde(default)]
    env: Vec<String>,
    #[serde(default)]
    workdir: Option<String>,
    #[serde(default)]
    timeout_ms: Option<u64>,
}

async fn create(State(state): State<Arc<AppState>>, body: Bytes) -> Response {
    let req: CreateRequest = if body.is_empty() {
        CreateRequest::default()
    } else {
        match serde_json::from_slice(&body) {
            Ok(req) => req,
            Err(err) => {
                return api_error(StatusCode::BAD_REQUEST, format!("invalid JSON body: {err}"))
            }
        }
    };

    if state.active.read().await.len() >= state.cfg.max_sandboxes {
        return api_error(StatusCode::TOO_MANY_REQUESTS, "sandbox limit reached");
    }

    let mut handle = match state.pool.acquire().await {
        Ok(handle) => handle,
        Err(err) => return api_error(StatusCode::SERVICE_UNAVAILABLE, err.to_string()),
    };

    let now = Utc::now();
    handle.created_at = now;
    let ttl = if req.ttl_seconds.is_finite() && req.ttl_seconds > 0.0 {
        Duration::try_from_secs_f64(req.ttl_seconds).unwrap_or(Duration::ZERO)
    } else {
        state.cfg.default_ttl
    };
    handle.expires_at = if ttl.is_zero() {
        None
    } else {
        chrono::Duration::from_std(ttl)
            .ok()
            .map(|duration| now + duration)
    };
    if let Some(labels) = req.labels {
        handle.labels = Some(labels);
    }

    state
        .active
        .write()
        .await
        .insert(handle.id.clone(), handle.clone());
    state.created.fetch_add(1, Ordering::Relaxed);
    (StatusCode::CREATED, Json(handle)).into_response()
}

async fn list(State(state): State<Arc<AppState>>) -> Response {
    let active = state.active.read().await;
    let out: Vec<Handle> = active.values().cloned().collect();
    Json(out).into_response()
}

async fn get_one(State(state): State<Arc<AppState>>, Path(id): Path<String>) -> Response {
    match state.active.read().await.get(&id) {
        Some(handle) => Json(handle.clone()).into_response(),
        None => api_error(StatusCode::NOT_FOUND, "sandbox not found"),
    }
}

async fn delete_one(State(state): State<Arc<AppState>>, Path(id): Path<String>) -> Response {
    if state.active.write().await.remove(&id).is_none() {
        return api_error(StatusCode::NOT_FOUND, "sandbox not found");
    }
    match state.pool.destroy(&id).await {
        Ok(()) | Err(SandboxError::NotFound) => {
            state.destroyed.fetch_add(1, Ordering::Relaxed);
            StatusCode::NO_CONTENT.into_response()
        }
        Err(err) => {
            state.errors.fetch_add(1, Ordering::Relaxed);
            api_error(StatusCode::BAD_GATEWAY, err.to_string())
        }
    }
}

async fn exec(State(state): State<Arc<AppState>>, Path(id): Path<String>, body: Bytes) -> Response {
    if !state.active.read().await.contains_key(&id) {
        return api_error(StatusCode::NOT_FOUND, "sandbox not found");
    }
    let req: ExecBody = match serde_json::from_slice(&body) {
        Ok(req) => req,
        Err(err) => return api_error(StatusCode::BAD_REQUEST, format!("invalid JSON body: {err}")),
    };
    if req.argv.is_empty() {
        return api_error(StatusCode::BAD_REQUEST, "argv is required");
    }

    let timeout = req
        .timeout_ms
        .map(Duration::from_millis)
        .unwrap_or(state.cfg.exec_timeout);
    let exec_req = ExecRequest {
        argv: req.argv,
        env: req.env,
        workdir: req.workdir,
    };
    let start = Instant::now();
    match tokio::time::timeout(timeout, state.driver.exec(&id, &exec_req)).await {
        Err(_) => {
            state.errors.fetch_add(1, Ordering::Relaxed);
            api_error(StatusCode::GATEWAY_TIMEOUT, "exec timed out")
        }
        Ok(Err(SandboxError::NotFound)) => {
            state.errors.fetch_add(1, Ordering::Relaxed);
            api_error(StatusCode::NOT_FOUND, "sandbox not found")
        }
        Ok(Err(err)) => {
            state.errors.fetch_add(1, Ordering::Relaxed);
            api_error(StatusCode::BAD_GATEWAY, err.to_string())
        }
        Ok(Ok(result)) => {
            state.execs.fetch_add(1, Ordering::Relaxed);
            Json(json!({
                "stdout": String::from_utf8_lossy(&result.stdout),
                "stderr": String::from_utf8_lossy(&result.stderr),
                "exit_code": result.exit_code,
                "duration_ms": start.elapsed().as_millis() as u64,
            }))
            .into_response()
        }
    }
}

async fn health(State(state): State<Arc<AppState>>) -> Response {
    Json(json!({"status": "ok", "driver": state.driver.name()})).into_response()
}

async fn metrics(State(state): State<Arc<AppState>>) -> Response {
    let pool = state.pool.stats();
    let active = state.active.read().await.len();
    let body = format!(
        "# HELP sysgate_sandboxes_created_total Total sandboxes handed out.
# TYPE sysgate_sandboxes_created_total counter
sysgate_sandboxes_created_total {}
# HELP sysgate_sandboxes_destroyed_total Total sandboxes destroyed.
# TYPE sysgate_sandboxes_destroyed_total counter
sysgate_sandboxes_destroyed_total {}
# HELP sysgate_exec_total Total exec requests served.
# TYPE sysgate_exec_total counter
sysgate_exec_total {}
# HELP sysgate_errors_total Total internal errors.
# TYPE sysgate_errors_total counter
sysgate_errors_total {}
# HELP sysgate_active_sandboxes Currently active sandboxes.
# TYPE sysgate_active_sandboxes gauge
sysgate_active_sandboxes {}
# HELP sysgate_pool_warm Currently warm sandboxes.
# TYPE sysgate_pool_warm gauge
sysgate_pool_warm {}
# HELP sysgate_pool_hits_total Warm pool hits.
# TYPE sysgate_pool_hits_total counter
sysgate_pool_hits_total {}
# HELP sysgate_pool_misses_total Warm pool misses.
# TYPE sysgate_pool_misses_total counter
sysgate_pool_misses_total {}
# HELP sysgate_uptime_seconds Process uptime.
# TYPE sysgate_uptime_seconds gauge
sysgate_uptime_seconds {:.0}
",
        state.created.load(Ordering::Relaxed),
        state.destroyed.load(Ordering::Relaxed),
        state.execs.load(Ordering::Relaxed),
        state.errors.load(Ordering::Relaxed),
        active,
        pool.warm,
        pool.hits,
        pool.misses,
        state.started.elapsed().as_secs_f64(),
    );
    (
        [(
            header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        body,
    )
        .into_response()
}

/// Background TTL reaper.
pub async fn reap_loop(state: Arc<AppState>) {
    let mut ticker = tokio::time::interval(state.cfg.reap_interval);
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        ticker.tick().await;
        reap_once(&state).await;
    }
}

async fn reap_once(state: &Arc<AppState>) {
    let now = Utc::now();
    let expired: Vec<String> = {
        let mut active = state.active.write().await;
        let expired: Vec<String> = active
            .iter()
            .filter(|(_, handle)| handle.expires_at.is_some_and(|at| at < now))
            .map(|(id, _)| id.clone())
            .collect();
        for id in &expired {
            active.remove(id);
        }
        expired
    };
    for id in expired {
        if let Err(err) = state.pool.destroy(&id).await {
            state.errors.fetch_add(1, Ordering::Relaxed);
            eprintln!("reap destroy failed for {id}: {err}");
        }
        state.destroyed.fetch_add(1, Ordering::Relaxed);
    }
}

fn api_error(status: StatusCode, message: impl Into<String>) -> Response {
    (status, Json(json!({ "error": message.into() }))).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::fake::FakeDriver;
    use crate::sandbox::Spec;
    use axum::body::Body;
    use axum::http::Request;
    use serde_json::Value;
    use tower::ServiceExt;

    fn test_app() -> (Router, Arc<AppState>) {
        let driver = Arc::new(Driver::Fake(FakeDriver::new()));
        let pool = Pool::new(driver.clone(), Spec::default(), 1);
        let state = AppState::new(driver, pool, Config::default());
        (router(state.clone()), state)
    }

    async fn body_json(response: Response) -> Value {
        let bytes = axum::body::to_bytes(response.into_body(), usize::MAX)
            .await
            .unwrap();
        serde_json::from_slice(&bytes).unwrap()
    }

    async fn request(app: &Router, method: &str, uri: &str, body: Option<&str>) -> Response {
        let request = Request::builder()
            .method(method)
            .uri(uri)
            .body(
                body.map(|body| Body::from(body.to_string()))
                    .unwrap_or_else(Body::empty),
            )
            .unwrap();
        app.clone().oneshot(request).await.unwrap()
    }

    #[tokio::test]
    async fn lifecycle() {
        let (app, _state) = test_app();

        let response = request(&app, "POST", "/v1/sandboxes", None).await;
        assert_eq!(response.status(), StatusCode::CREATED);
        let body = body_json(response).await;
        let id = body["id"].as_str().unwrap().to_string();

        let response = request(
            &app,
            "POST",
            &format!("/v1/sandboxes/{id}/exec"),
            Some(r#"{"argv":["echo","hi"]}"#),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body = body_json(response).await;
        assert_eq!(body["stdout"], "fake:echo hi");

        let response = request(&app, "DELETE", &format!("/v1/sandboxes/{id}"), None).await;
        assert_eq!(response.status(), StatusCode::NO_CONTENT);

        let response = request(&app, "GET", &format!("/v1/sandboxes/{id}"), None).await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn list_and_limit() {
        let driver = Arc::new(Driver::Fake(FakeDriver::new()));
        let pool = Pool::new(driver.clone(), Spec::default(), 1);
        let state = AppState::new(
            driver,
            pool,
            Config {
                max_sandboxes: 1,
                ..Config::default()
            },
        );
        let app = router(state);

        let response = request(&app, "POST", "/v1/sandboxes", None).await;
        assert_eq!(response.status(), StatusCode::CREATED);

        let response = request(&app, "POST", "/v1/sandboxes", None).await;
        assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);

        let response = request(&app, "GET", "/v1/sandboxes", None).await;
        assert_eq!(response.status(), StatusCode::OK);
        let body = body_json(response).await;
        assert_eq!(body.as_array().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn exec_validation() {
        let (app, _state) = test_app();
        let body = body_json(request(&app, "POST", "/v1/sandboxes", None).await).await;
        let id = body["id"].as_str().unwrap().to_string();

        let response = request(
            &app,
            "POST",
            &format!("/v1/sandboxes/{id}/exec"),
            Some("{}"),
        )
        .await;
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);

        let response = request(
            &app,
            "POST",
            "/v1/sandboxes/missing/exec",
            Some(r#"{"argv":["true"]}"#),
        )
        .await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn health_and_metrics() {
        let (app, _state) = test_app();

        let response = request(&app, "GET", "/healthz", None).await;
        assert_eq!(response.status(), StatusCode::OK);

        let response = request(&app, "GET", "/metrics", None).await;
        assert_eq!(response.status(), StatusCode::OK);
        let bytes = axum::body::to_bytes(response.into_body(), usize::MAX)
            .await
            .unwrap();
        let text = String::from_utf8(bytes.to_vec()).unwrap();
        assert!(text.contains("sysgate_sandboxes_created_total"));
    }

    #[tokio::test]
    async fn reaps_expired() {
        let (app, state) = test_app();
        let body = body_json(
            request(
                &app,
                "POST",
                "/v1/sandboxes",
                Some(r#"{"ttl_seconds":0.03}"#),
            )
            .await,
        )
        .await;
        let id = body["id"].as_str().unwrap().to_string();

        tokio::time::sleep(Duration::from_millis(60)).await;
        reap_once(&state).await;

        let response = request(&app, "GET", &format!("/v1/sandboxes/{id}"), None).await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }
}
