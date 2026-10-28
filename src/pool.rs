//! Warm pool of pre-created sandboxes so gateway requests can be served in
//! microseconds instead of paying runsc boot latency.

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::{mpsc, Notify};
use tokio::task::JoinHandle;

use crate::sandbox::{Driver, Handle, Result, Spec};

#[derive(Debug, Clone, Copy, Default)]
pub struct Stats {
    pub warm: usize,
    pub hits: u64,
    pub misses: u64,
}

pub struct Pool {
    driver: Arc<Driver>,
    spec: Spec,
    tx: mpsc::Sender<Handle>,
    rx: tokio::sync::Mutex<mpsc::Receiver<Handle>>,
    refill: Mutex<Option<JoinHandle<()>>>,
    shutdown: Notify,
    closed: AtomicBool,
    hits: AtomicU64,
    misses: AtomicU64,
    created: AtomicU64,
    errors: AtomicU64,
}

impl Pool {
    /// Starts a pool of the given size. Size 0 disables warming; `acquire`
    /// then creates sandboxes on demand.
    pub fn new(driver: Arc<Driver>, spec: Spec, size: usize) -> Arc<Self> {
        let (tx, rx) = mpsc::channel(size.max(1));
        let pool = Arc::new(Self {
            driver,
            spec,
            tx,
            rx: tokio::sync::Mutex::new(rx),
            refill: Mutex::new(None),
            shutdown: Notify::new(),
            closed: AtomicBool::new(false),
            hits: AtomicU64::new(0),
            misses: AtomicU64::new(0),
            created: AtomicU64::new(0),
            errors: AtomicU64::new(0),
        });
        if size > 0 {
            let worker = pool.clone();
            let handle = tokio::spawn(async move { worker.refill_loop().await });
            *pool.refill.lock().expect("pool refill lock") = Some(handle);
        }
        pool
    }

    async fn refill_loop(&self) {
        let mut backoff = Duration::from_millis(100);
        loop {
            if self.closed.load(Ordering::Acquire) {
                return;
            }
            match self.driver.create(&self.spec).await {
                Ok(handle) => {
                    if self.closed.load(Ordering::Acquire) {
                        let _ = self.driver.destroy(&handle.id).await;
                        return;
                    }
                    let permit = tokio::select! {
                        permit = self.tx.reserve() => match permit {
                            Ok(permit) => permit,
                            Err(_) => {
                                let _ = self.driver.destroy(&handle.id).await;
                                return;
                            }
                        },
                        _ = self.shutdown.notified() => {
                            let _ = self.driver.destroy(&handle.id).await;
                            return;
                        }
                    };
                    permit.send(handle);
                    backoff = Duration::from_millis(100);
                }
                Err(err) => {
                    self.errors.fetch_add(1, Ordering::Relaxed);
                    eprintln!("warm pool create failed: {err} (retry in {backoff:?})");
                    tokio::select! {
                        _ = tokio::time::sleep(backoff) => {}
                        _ = self.shutdown.notified() => return,
                    }
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    }

    /// Hands out a warm sandbox if one is ready, otherwise creates one on
    /// demand.
    pub async fn acquire(&self) -> Result<Handle> {
        {
            let mut rx = self.rx.lock().await;
            if let Ok(handle) = rx.try_recv() {
                self.hits.fetch_add(1, Ordering::Relaxed);
                return Ok(handle);
            }
        }
        self.misses.fetch_add(1, Ordering::Relaxed);
        match self.driver.create(&self.spec).await {
            Ok(handle) => {
                self.created.fetch_add(1, Ordering::Relaxed);
                Ok(handle)
            }
            Err(err) => {
                self.errors.fetch_add(1, Ordering::Relaxed);
                Err(err)
            }
        }
    }

    /// Tears down a sandbox handed out by [`Pool::acquire`].
    pub async fn destroy(&self, id: &str) -> Result<()> {
        self.driver.destroy(id).await
    }

    pub fn stats(&self) -> Stats {
        Stats {
            warm: self.tx.max_capacity() - self.tx.capacity(),
            hits: self.hits.load(Ordering::Relaxed),
            misses: self.misses.load(Ordering::Relaxed),
        }
    }

    /// Stops refilling and destroys every warm sandbox.
    pub async fn close(&self) {
        self.closed.store(true, Ordering::Release);
        self.shutdown.notify_one();
        let refill = self.refill.lock().expect("pool refill lock").take();
        if let Some(handle) = refill {
            let _ = tokio::time::timeout(Duration::from_secs(5), handle).await;
        }
        loop {
            let handle = {
                let mut rx = self.rx.lock().await;
                rx.try_recv()
            };
            match handle {
                Ok(handle) => {
                    let _ = self.driver.destroy(&handle.id).await;
                }
                Err(_) => break,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sandbox::fake::FakeDriver;

    fn fake_pool(size: usize) -> Arc<Pool> {
        Pool::new(
            Arc::new(Driver::Fake(FakeDriver::new())),
            Spec::default(),
            size,
        )
    }

    async fn wait_warm(pool: &Pool, want: usize) {
        for _ in 0..200 {
            if pool.stats().warm >= want {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("pool did not warm to {want} (got {})", pool.stats().warm);
    }

    #[tokio::test]
    async fn warm_acquire() {
        let pool = fake_pool(2);
        wait_warm(&pool, 2).await;

        let first = pool.acquire().await.unwrap();
        let second = pool.acquire().await.unwrap();
        assert_ne!(first.id, second.id);
        assert_eq!(pool.stats().hits, 2);

        wait_warm(&pool, 2).await;
        pool.close().await;
    }

    #[tokio::test]
    async fn on_demand() {
        let pool = fake_pool(0);
        let handle = pool.acquire().await.unwrap();
        assert!(!handle.id.is_empty());
        assert_eq!(pool.stats().misses, 1);
        pool.close().await;
    }

    #[tokio::test]
    async fn close_destroys_warm() {
        let pool = fake_pool(3);
        wait_warm(&pool, 3).await;
        pool.close().await;
        assert_eq!(pool.stats().warm, 0);
    }
}
