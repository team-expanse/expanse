"""A test HTTP backend: answers every GET with its tag and the X-Forwarded-For it saw.

Usage: tag_backend.py PORT TAG
"""

import socket
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"{sys.argv[2]} xff={self.headers.get('X-Forwarded-For', '-')}\n".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class DualStackServer(ThreadingHTTPServer):
    """Listens on IPv6 and IPv4: test hosts resolve node names to both."""

    address_family = socket.AF_INET6

    def server_bind(self):
        self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        super().server_bind()


DualStackServer(("::", int(sys.argv[1])), Handler).serve_forever()
