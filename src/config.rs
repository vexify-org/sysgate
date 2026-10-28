use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct Config {
    pub listen: String,
    pub driver: String,
    pub runsc_bin: String,
    pub root: PathBuf,
    pub rootfs: Option<String>,
    pub network: String,
    pub pool_size: usize,
    pub max_sandboxes: usize,
    pub default_ttl: Duration,
    pub exec_timeout: Duration,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            listen: env("SYSGATE_LISTEN", ":8080"),
            driver: env("SYSGATE_DRIVER", "fake"),
            runsc_bin: env("SYSGATE_RUNSC_BIN", "runsc"),
            root: PathBuf::from(env("SYSGATE_ROOT", "/var/lib/sysgate")),
            rootfs: std::env::var("SYSGATE_ROOTFS")
                .ok()
                .filter(|value| !value.is_empty()),
            network: env("SYSGATE_NETWORK", "none"),
            pool_size: env_parse("SYSGATE_POOL_SIZE", 4),
            max_sandboxes: env_parse("SYSGATE_MAX_SANDBOXES", 256),
            default_ttl: env_duration("SYSGATE_DEFAULT_TTL", Duration::from_secs(600)),
            exec_timeout: env_duration("SYSGATE_EXEC_TIMEOUT", Duration::from_secs(30)),
        }
    }
}

fn env(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_parse<T: std::str::FromStr>(key: &str, default: T) -> T {
    std::env::var(key)
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(default)
}

fn env_duration(key: &str, default: Duration) -> Duration {
    std::env::var(key)
        .ok()
        .and_then(|value| parse_duration(&value))
        .unwrap_or(default)
}

fn parse_duration(value: &str) -> Option<Duration> {
    let value = value.trim();
    if let Ok(secs) = value.parse::<u64>() {
        return Some(Duration::from_secs(secs));
    }
    let split = value.find(|c: char| !c.is_ascii_digit())?;
    let (number, unit) = value.split_at(split);
    let number: u64 = number.parse().ok()?;
    match unit {
        "ms" => Some(Duration::from_millis(number)),
        "s" => Some(Duration::from_secs(number)),
        "m" => Some(Duration::from_secs(number * 60)),
        "h" => Some(Duration::from_secs(number * 3600)),
        _ => None,
    }
}
