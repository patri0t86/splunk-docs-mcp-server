"""SQLite crawl state for incremental re-crawls.

A page is skipped when its sitemap <lastmod> matches what we stored and the
output file still exists; everything else is (re)fetched. Deleting the DB (or
passing --force) triggers a full crawl.
"""

from __future__ import annotations

import sqlite3
from datetime import datetime, timezone
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS pages (
    url TEXT PRIMARY KEY,
    product TEXT NOT NULL,
    version TEXT,
    sitemap_lastmod TEXT,
    content_hash TEXT,
    file_path TEXT,
    fetched_at TEXT,
    http_status INTEGER
);
CREATE INDEX IF NOT EXISTS idx_pages_product ON pages(product);
"""


class CrawlState:
    def __init__(self, db_path: Path, output_dir: Path):
        db_path.parent.mkdir(parents=True, exist_ok=True)
        self._output_dir = output_dir
        self._conn = sqlite3.connect(db_path)
        self._conn.row_factory = sqlite3.Row
        self._conn.executescript(SCHEMA)

    def close(self) -> None:
        self._conn.close()

    def needs_fetch(self, url: str, lastmod: str | None) -> bool:
        row = self._conn.execute(
            "SELECT sitemap_lastmod, file_path, http_status FROM pages WHERE url = ?", (url,)
        ).fetchone()
        if row is None or row["http_status"] != 200 or not row["file_path"]:
            return True
        if lastmod is None or row["sitemap_lastmod"] != lastmod:
            return True
        # file_path is stored relative to output_dir so the data directory can
        # be moved between machines; absolute paths from older DBs still work.
        path = Path(row["file_path"])
        if not path.is_absolute():
            path = self._output_dir / path
        return not path.exists()

    def record(
        self,
        url: str,
        *,
        product: str,
        version: str | None,
        lastmod: str | None,
        content_hash: str | None,
        file_path: str | None,
        http_status: int,
    ) -> None:
        self._conn.execute(
            """INSERT INTO pages (url, product, version, sitemap_lastmod, content_hash,
                                  file_path, fetched_at, http_status)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?)
               ON CONFLICT(url) DO UPDATE SET
                   product = excluded.product,
                   version = excluded.version,
                   sitemap_lastmod = excluded.sitemap_lastmod,
                   content_hash = excluded.content_hash,
                   file_path = excluded.file_path,
                   fetched_at = excluded.fetched_at,
                   http_status = excluded.http_status""",
            (
                url,
                product,
                version,
                lastmod,
                content_hash,
                file_path,
                datetime.now(timezone.utc).isoformat(timespec="seconds"),
                http_status,
            ),
        )
        self._conn.commit()

    def forget_files(self, rel_paths: list[str]) -> int:
        """Drop state rows for deleted output files (paths relative to output_dir)."""
        removed = 0
        # SQLite caps bound parameters per statement; chunk to stay under it.
        for i in range(0, len(rel_paths), 500):
            batch = rel_paths[i : i + 500]
            cur = self._conn.execute(
                f"DELETE FROM pages WHERE file_path IN ({','.join('?' * len(batch))})",
                batch,
            )
            removed += cur.rowcount
        self._conn.commit()
        return removed

    def summary(self) -> list[sqlite3.Row]:
        return self._conn.execute(
            """SELECT product, version,
                      COUNT(*) AS pages,
                      SUM(CASE WHEN http_status = 200 THEN 1 ELSE 0 END) AS ok,
                      MAX(fetched_at) AS last_fetch
               FROM pages GROUP BY product, version ORDER BY product, version"""
        ).fetchall()
