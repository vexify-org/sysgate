.PHONY: build release test clippy fmt run clean

build:
	cargo build

release:
	cargo build --release

test:
	cargo test

clippy:
	cargo clippy --all-targets -- -D warnings

fmt:
	cargo fmt

run:
	cargo run

clean:
	cargo clean
