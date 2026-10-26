#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Relay TCP connections from 127.0.0.1:LISTEN_PORT to HOST:PORT.

hack/ci-run-tempest.sh starts it in place of a kubectl port-forward for an API
that may answer a streamed upload before it reads the body: Glance refuses an
over-quota upload with 413 that way. The kubelet's write into the closed socket
then fails, and client-go closes the whole forward on that error, so every later
request of the leg meets a refused connection. This relay opens one upstream
connection per client connection, hands the response through, and ends only
that pair when either side closes or resets it.
"""
from __future__ import annotations

import asyncio
import sys


async def pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    # Copy until EOF or a reset, then pass the end of the stream on, so a
    # response the upstream sent before it reset the connection still reaches
    # the client in full.
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except OSError:
        pass
    finally:
        try:
            if writer.can_write_eof():
                writer.write_eof()
        except OSError:
            pass


async def main() -> int:
    if len(sys.argv) != 4:
        print("usage: tcp-relay.py LISTEN_PORT HOST PORT", file=sys.stderr)
        return 2
    listen_port, host, port = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])

    async def handle(creader: asyncio.StreamReader, cwriter: asyncio.StreamWriter) -> None:
        try:
            ureader, uwriter = await asyncio.open_connection(host, port)
        except OSError as exc:
            print(f"connect to {host}:{port} failed: {exc}", file=sys.stderr, flush=True)
            cwriter.close()
            return
        await asyncio.gather(pipe(creader, uwriter), pipe(ureader, cwriter))
        for writer in (cwriter, uwriter):
            writer.close()

    server = await asyncio.start_server(handle, "127.0.0.1", listen_port)
    print(f"Relaying from 127.0.0.1:{listen_port} -> {host}:{port}", flush=True)
    async with server:
        await server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
