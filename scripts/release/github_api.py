# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""GitHub REST client for the release scripts."""

import json
import os
import shutil
from itertools import count
from pathlib import Path
from urllib.parse import urlencode
from urllib.request import Request, urlopen

BINARY = "application/octet-stream"


class GitHub:
    def __init__(self):
        repo = os.environ.get("GITHUB_REPOSITORY", "C5C3/cobaltcore")
        api = os.environ.get("GITHUB_API_URL", "https://api.github.com")
        server = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
        self.api = f"{api}/repos/{repo}"
        self.repo_url = f"{server}/{repo}"
        self.token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
        if not self.token:
            raise SystemExit("Set GH_TOKEN to access GitHub release notes and assets")

    def request(self, url, *, method=None, data=None, headers=None):
        request = Request(
            url,
            data=data,
            method=method,
            headers={
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
                **(headers or {}),
            },
        )
        # Asset downloads can redirect to storage outside the GitHub API.
        request.add_unredirected_header("Authorization", f"Bearer {self.token}")
        return urlopen(request, timeout=60)

    def json(self, path, *, method="GET", body=None):
        data = None if body is None else json.dumps(body).encode("utf-8")
        with self.request(
            f"{self.api}/{path}",
            method=method,
            data=data,
            headers={"Content-Type": "application/json"} if data is not None else None,
        ) as response:
            return json.load(response)

    def items(self, path):
        for page in count(1):
            items = self.json(f"{path}?per_page=100&page={page}")
            yield from items
            if len(items) < 100:
                return

    def release(self, tag):
        # The list endpoint includes drafts, which must survive failed runs.
        return next(
            (release for release in self.items("releases") if release["tag_name"] == tag),
            None,
        )

    def assets(self, release):
        assets = {}
        for asset in self.items(f"releases/{release['id']}/assets"):
            name = asset["name"]
            if name in {"", ".", ".."} or Path(name).name != name or name in assets:
                raise SystemExit(f"Invalid or duplicate release asset name: {name!r}")
            assets[name] = asset
        return assets

    def download(self, asset, path):
        url = f"{self.api}/releases/assets/{asset['id']}"
        with self.request(url, headers={"Accept": BINARY}) as response:
            with path.open("xb") as file:
                shutil.copyfileobj(response, file)

    def upload(self, release, path):
        url = release["upload_url"].split("{", 1)[0]
        with path.open("rb") as file:
            with self.request(
                f"{url}?{urlencode({'name': path.name})}",
                method="POST",
                data=file,
                headers={
                    "Content-Type": BINARY,
                    "Content-Length": str(path.stat().st_size),
                },
            ) as response:
                response.read()
