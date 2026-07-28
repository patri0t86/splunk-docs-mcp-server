"""Configuration loading and version parsing."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path

import yaml

VERSION_RE = re.compile(r"^\d+(?:\.\d+)+$")
VERSION_PREFIX_RE = re.compile(r"^(\d+(?:\.\d+)+)(?:-|$)")


def parse_version(text: str) -> tuple[int, ...]:
    """Parse "10.2" into (10, 2) so comparisons are numeric, not lexical."""
    return tuple(int(part) for part in text.split("."))


@dataclass(frozen=True)
class ProductConfig:
    name: str
    sitemap: str
    path_prefix: str
    enabled: bool = True
    min_version: tuple[int, ...] | None = None
    versions: frozenset[str] | None = None  # explicit allowlist, overrides min_version
    include_unversioned: bool = False
    spa: bool = False  # single-page app — bypasses sitemap/fetch pipeline, uses Playwright
    exclude_paths: frozenset[str] = frozenset()

    def accepts_version(self, version: str | None) -> bool:
        if version is None:
            return self.include_unversioned
        if self.versions is not None:
            return version in self.versions
        if self.min_version is not None:
            return parse_version(version) >= self.min_version
        return True


@dataclass(frozen=True)
class CrawlerConfig:
    user_agent: str
    rate_limit_per_sec: float
    concurrency: int
    timeout_seconds: float
    retries: int
    output_dir: Path
    state_db: Path
    products: dict[str, ProductConfig] = field(default_factory=dict)


def load_config(path: Path) -> CrawlerConfig:
    raw = yaml.safe_load(path.read_text())
    base = path.parent

    products: dict[str, ProductConfig] = {}
    for name, entry in (raw.get("products") or {}).items():
        min_version = entry.get("min_version")
        versions = entry.get("versions")
        products[name] = ProductConfig(
            name=name,
            sitemap=entry.get("sitemap", ""),
            path_prefix=entry.get("path_prefix", "/"),
            enabled=bool(entry.get("enabled", True)),
            min_version=parse_version(str(min_version)) if min_version else None,
            versions=frozenset(str(v) for v in versions) if versions else None,
            include_unversioned=bool(entry.get("include_unversioned", False)),
            spa=bool(entry.get("spa", False)),
            exclude_paths=frozenset(str(path) for path in (entry.get("exclude_paths") or [])),
        )

    return CrawlerConfig(
        user_agent=raw["user_agent"],
        rate_limit_per_sec=float(raw.get("rate_limit_per_sec", 3)),
        concurrency=int(raw.get("concurrency", 4)),
        timeout_seconds=float(raw.get("timeout_seconds", 30)),
        retries=int(raw.get("retries", 3)),
        output_dir=(base / raw["output_dir"]).resolve(),
        state_db=(base / raw["state_db"]).resolve(),
        products=products,
    )
