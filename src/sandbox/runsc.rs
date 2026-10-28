//! gVisor sandbox driver built on the `runsc` CLI.

use std::path::{Path, PathBuf};
use std::process::Stdio;

use chrono::Utc;
use serde::Serialize;
use tokio::process::Command;

use super::{Error, ExecRequest, ExecResult, Handle, Result, Spec};

pub struct RunscDriver {
    bin: String,
    root: PathBuf,
    rootfs: Option<String>,
    network: String,
}

impl RunscDriver {
    pub fn new(
        bin: impl Into<String>,
        root: impl Into<PathBuf>,
        rootfs: Option<String>,
        network: impl Into<String>,
    ) -> Result<Self> {
        let root = root.into();
        std::fs::create_dir_all(&root)
            .map_err(|e| Error::Io(format!("create state root {}: {e}", root.display())))?;
        Ok(Self {
            bin: bin.into(),
            root,
            rootfs,
            network: network.into(),
        })
    }

    pub async fn create(&self, spec: &Spec) -> Result<Handle> {
        let rootfs = spec
            .rootfs
            .clone()
            .or_else(|| self.rootfs.clone())
            .ok_or_else(|| {
                Error::Invalid("runsc: rootfs is required (set SYSGATE_ROOTFS)".into())
            })?;
        if !Path::new(&rootfs).is_dir() {
            return Err(Error::Invalid(format!(
                "runsc: rootfs {rootfs:?} is not a directory"
            )));
        }

        let id = new_id();
        let dir = self.root.join("sandboxes").join(&id);
        let bundle = dir.join("bundle");
        std::fs::create_dir_all(&bundle).map_err(|e| Error::Io(format!("create bundle: {e}")))?;

        let mut init = spec.init.clone();
        if init.is_empty() {
            init = vec![
                "/bin/sh".into(),
                "-c".into(),
                "while :; do sleep 3600; done".into(),
            ];
        }
        let cfg = build_oci(&id, &rootfs, &init, &spec.env);
        let data = serde_json::to_vec_pretty(&cfg)
            .map_err(|e| Error::Io(format!("encode config.json: {e}")))?;
        std::fs::write(bundle.join("config.json"), data)
            .map_err(|e| Error::Io(format!("write config.json: {e}")))?;

        let stderr_path = dir.join("runsc.stderr");
        let stderr = std::fs::File::create(&stderr_path)
            .map_err(|e| Error::Io(format!("create runsc stderr log: {e}")))?;
        let status = Command::new(&self.bin)
            .arg("--root")
            .arg(&self.root)
            .arg("--network")
            .arg(&self.network)
            .arg("run")
            .arg("-detach")
            .arg("--bundle")
            .arg(&bundle)
            .arg(&id)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::from(stderr))
            .status()
            .await
            .map_err(|e| Error::Io(format!("runsc run: {e}")))?;
        if !status.success() {
            let message = std::fs::read_to_string(&stderr_path).unwrap_or_default();
            let _ = std::fs::remove_dir_all(&dir);
            return Err(Error::Io(format!("runsc run: {}", message.trim())));
        }

        let now = Utc::now();
        Ok(Handle {
            id,
            driver: "runsc".into(),
            created_at: now,
            expires_at: spec
                .ttl
                .and_then(|ttl| chrono::Duration::from_std(ttl).ok())
                .map(|d| now + d),
            labels: None,
        })
    }

    pub async fn exec(&self, id: &str, req: &ExecRequest) -> Result<ExecResult> {
        if !valid_id(id) {
            return Err(Error::Invalid(format!("runsc: invalid sandbox id {id:?}")));
        }
        if req.argv.is_empty() {
            return Err(Error::Invalid("runsc: argv is required".into()));
        }

        let mut cmd = Command::new(&self.bin);
        cmd.arg("--root").arg(&self.root).arg("exec");
        if let Some(cwd) = &req.workdir {
            cmd.arg("--cwd").arg(cwd);
        }
        for env in &req.env {
            cmd.arg("--env").arg(env);
        }
        cmd.arg(id);
        for arg in &req.argv {
            cmd.arg(arg);
        }
        cmd.kill_on_drop(true);
        cmd.stdin(Stdio::null());

        let output = cmd
            .output()
            .await
            .map_err(|e| Error::Io(format!("runsc exec: {e}")))?;
        Ok(ExecResult {
            stdout: output.stdout,
            stderr: output.stderr,
            exit_code: output.status.code().unwrap_or(-1),
        })
    }

    pub async fn destroy(&self, id: &str) -> Result<()> {
        if !valid_id(id) {
            return Err(Error::Invalid(format!("runsc: invalid sandbox id {id:?}")));
        }
        let _ = Command::new(&self.bin)
            .arg("--root")
            .arg(&self.root)
            .arg("kill")
            .arg(id)
            .arg("KILL")
            .output()
            .await;
        let out = Command::new(&self.bin)
            .arg("--root")
            .arg(&self.root)
            .arg("delete")
            .arg("-force")
            .arg(id)
            .output()
            .await;
        let _ = std::fs::remove_dir_all(self.root.join("sandboxes").join(id));
        match out {
            Ok(o) if o.status.success() => Ok(()),
            Ok(o) => {
                let msg = String::from_utf8_lossy(&o.stderr);
                if msg.contains("not found") || msg.contains("does not exist") {
                    Ok(())
                } else {
                    Err(Error::Io(format!("runsc delete: {}", msg.trim())))
                }
            }
            Err(e) => Err(Error::Io(format!("runsc delete: {e}"))),
        }
    }
}

fn new_id() -> String {
    format!("sg-{}", uuid::Uuid::new_v4().simple())
}

fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

#[derive(Serialize)]
struct OciConfig {
    #[serde(rename = "ociVersion")]
    oci_version: &'static str,
    process: OciProcess,
    root: OciRoot,
    hostname: String,
    mounts: Vec<OciMount>,
    linux: OciLinux,
}

#[derive(Serialize)]
struct OciProcess {
    terminal: bool,
    user: OciUser,
    args: Vec<String>,
    env: Vec<String>,
    cwd: String,
}

#[derive(Serialize)]
struct OciUser {
    uid: u32,
    gid: u32,
}

#[derive(Serialize)]
struct OciRoot {
    path: String,
    readonly: bool,
}

#[derive(Serialize)]
struct OciMount {
    destination: String,
    #[serde(rename = "type")]
    kind: String,
    source: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    options: Option<Vec<String>>,
}

#[derive(Serialize)]
struct OciLinux {
    namespaces: Vec<OciNamespace>,
}

#[derive(Serialize)]
struct OciNamespace {
    #[serde(rename = "type")]
    kind: String,
}

fn build_oci(id: &str, rootfs: &str, init: &[String], extra_env: &[String]) -> OciConfig {
    let mut env = vec![
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin".to_string(),
        format!("HOSTNAME={id}"),
    ];
    env.extend(extra_env.iter().cloned());

    let mount =
        |destination: &str, kind: &str, source: &str, options: Option<Vec<String>>| OciMount {
            destination: destination.into(),
            kind: kind.into(),
            source: source.into(),
            options,
        };

    OciConfig {
        oci_version: "1.0.2",
        process: OciProcess {
            terminal: false,
            user: OciUser { uid: 0, gid: 0 },
            args: init.to_vec(),
            env,
            cwd: "/".into(),
        },
        root: OciRoot {
            path: rootfs.into(),
            readonly: false,
        },
        hostname: id.into(),
        mounts: vec![
            mount("/proc", "proc", "proc", None),
            mount(
                "/dev",
                "tmpfs",
                "tmpfs",
                Some(vec![
                    "nosuid".into(),
                    "strictatime".into(),
                    "mode=755".into(),
                    "size=65536k".into(),
                ]),
            ),
            mount(
                "/sys",
                "sysfs",
                "sysfs",
                Some(vec![
                    "nosuid".into(),
                    "noexec".into(),
                    "nodev".into(),
                    "ro".into(),
                ]),
            ),
            mount(
                "/tmp",
                "tmpfs",
                "tmpfs",
                Some(vec![
                    "nosuid".into(),
                    "nodev".into(),
                    "mode=1777".into(),
                    "size=64m".into(),
                ]),
            ),
        ],
        linux: OciLinux {
            namespaces: ["pid", "network", "ipc", "uts", "mount"]
                .into_iter()
                .map(|kind| OciNamespace { kind: kind.into() })
                .collect(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn valid_ids() {
        for id in ["sg-abc123", "abc", "a-b-c"] {
            assert!(valid_id(id), "valid_id({id:?}) = false");
        }
        for id in ["", "../etc", "SG-ABC", "a/b", "a b"] {
            assert!(!valid_id(id), "valid_id({id:?}) = true");
        }
    }

    #[test]
    fn oci_config_fields() {
        let cfg = build_oci(
            "sg-abc",
            "/srv/rootfs",
            &["/bin/sh".to_string()],
            &["FOO=bar".to_string()],
        );
        let v = serde_json::to_value(&cfg).unwrap();
        assert_eq!(v["ociVersion"], "1.0.2");
        assert_eq!(v["process"]["args"][0], "/bin/sh");
        assert_eq!(v["root"]["path"], "/srv/rootfs");
        assert!(v["process"]["env"]
            .as_array()
            .unwrap()
            .iter()
            .any(|e| e.as_str() == Some("FOO=bar")));
    }

    #[tokio::test]
    async fn create_requires_rootfs() {
        let dir = std::env::temp_dir().join(format!("sysgate-test-{}", uuid::Uuid::new_v4()));
        let driver = RunscDriver::new("runsc", &dir, None, "none").unwrap();
        let err = driver.create(&Spec::default()).await.unwrap_err();
        assert!(err.to_string().contains("rootfs"));
        let _ = std::fs::remove_dir_all(dir);
    }
}
