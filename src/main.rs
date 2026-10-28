mod config;
mod gateway;
mod pool;
mod sandbox;

use std::sync::Arc;
use std::time::Duration;

use crate::sandbox::{Driver, Spec};

#[tokio::main]
async fn main() {
    let cfg = config::Config::from_env();

    let driver = match cfg.driver.as_str() {
        "fake" => Driver::Fake(sandbox::fake::FakeDriver::new()),
        "runsc" => match sandbox::runsc::RunscDriver::new(
            cfg.runsc_bin.clone(),
            cfg.root.clone(),
            cfg.rootfs.clone(),
            cfg.network.clone(),
        ) {
            Ok(driver) => Driver::Runsc(driver),
            Err(err) => {
                eprintln!("init runsc driver: {err}");
                std::process::exit(1);
            }
        },
        other => {
            eprintln!("unknown driver {other:?} (want fake|runsc)");
            std::process::exit(1);
        }
    };
    let driver = Arc::new(driver);

    let pool = pool::Pool::new(
        driver.clone(),
        Spec {
            rootfs: cfg.rootfs.clone(),
            ..Spec::default()
        },
        cfg.pool_size,
    );
    let state = gateway::AppState::new(
        driver.clone(),
        pool.clone(),
        gateway::Config {
            default_ttl: cfg.default_ttl,
            max_sandboxes: cfg.max_sandboxes,
            exec_timeout: cfg.exec_timeout,
            reap_interval: Duration::from_secs(1),
        },
    );
    let reaper = tokio::spawn(gateway::reap_loop(state.clone()));

    let listener = match tokio::net::TcpListener::bind(&cfg.listen).await {
        Ok(listener) => listener,
        Err(err) => {
            eprintln!("bind {}: {err}", cfg.listen);
            std::process::exit(1);
        }
    };
    println!(
        "sysgate listening on {} driver={} pool={}",
        cfg.listen,
        driver.name(),
        cfg.pool_size
    );

    let app = gateway::router(state);
    if let Err(err) = axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await
    {
        eprintln!("server error: {err}");
    }

    reaper.abort();
    pool.close().await;
}

async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c()
            .await
            .expect("install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
}
