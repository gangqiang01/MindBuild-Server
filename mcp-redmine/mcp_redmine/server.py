import os, yaml, pathlib, json, uuid, base64
from urllib.parse import urljoin

import httpx
from mcp.server.mcpserver import MCPServer, Image, Context
from mcp.server.mcpserver.utilities.logging import get_logger

### Constants ###

VERSION = "2026.10.15.204909"

# Load OpenAPI spec
current_dir = pathlib.Path(__file__).parent
with open(current_dir / 'redmine_openapi.yml') as f:
    SPEC = yaml.safe_load(f)

# Constants from environment
# REDMINE_URL is only the fallback base URL. On HTTP transports a client can target
# its own Redmine instance per request via the X-Redmine-URL header (see
# resolve_redmine_url), so the env var is optional in shared/multi-tenant deployments.
REDMINE_URL = (os.environ.get('REDMINE_URL', '').rstrip('/') + '/') if os.environ.get('REDMINE_URL') else ''
REDMINE_API_KEY = os.environ.get('REDMINE_API_KEY', '')  # optional: HTTP clients can send X-Redmine-API-Key per request
REDMINE_RESPONSE_FORMAT = os.environ.get('REDMINE_RESPONSE_FORMAT', 'yaml').lower()

# Custom headers (format: "Header1: Value1, Header2: Value2")
REDMINE_HEADERS = {}
if custom_headers := os.environ.get('REDMINE_HEADERS', ''):
    for header in custom_headers.split(','):
        if ':' in header:
            key, value = header.split(':', 1)
            REDMINE_HEADERS[key.strip()] = value.strip()

# Allowed directories for upload/download (secure by default - disabled if not set)
REDMINE_ALLOWED_DIRECTORIES = [
    pathlib.Path(d.strip()).resolve()
    for d in os.environ.get('REDMINE_ALLOWED_DIRECTORIES', '').split(',')
    if d.strip()
]

# SSL verification (disabled only when explicitly set to "1")
REDMINE_DANGEROUSLY_ACCEPT_INVALID_CERTS = os.environ.get('REDMINE_DANGEROUSLY_ACCEPT_INVALID_CERTS') == '1'

# Custom CA bundle for private certificate chains (path to a ca.crt / bundle file)
REDMINE_CA_BUNDLE = os.environ.get('REDMINE_CA_BUNDLE', '')

# Read-only mode (enabled when set to "1") - only GET requests are allowed
REDMINE_READ_ONLY = os.environ.get('REDMINE_READ_ONLY') == '1'

# Max decoded size accepted by redmine_upload_content, in bytes. The payload
# travels as base64 inside a JSON tool argument (Go chat attachments are read
# server-side, never exposed to the model as raw bytes), so both the encoded
# length and the decoded size are gated. Default 10 MiB matches Redmine's own
# default attachment limit; override via REDMINE_UPLOAD_MAX_BYTES.
DEFAULT_UPLOAD_MAX_BYTES = 10 * 1024 * 1024


def _env_positive_int(name: str, default: int) -> int:
    raw = os.environ.get(name, '')
    try:
        value = int(raw)
    except (TypeError, ValueError):
        return default
    return value if value > 0 else default


REDMINE_UPLOAD_MAX_BYTES = _env_positive_int('REDMINE_UPLOAD_MAX_BYTES', DEFAULT_UPLOAD_MAX_BYTES)


def resolve_redmine_url(ctx: Context = None) -> tuple[str, str]:
    """Per-request Redmine base URL, mirroring the X-Redmine-API-Key pattern:
    an HTTP client sends X-Redmine-URL to point this shared server at its own
    Redmine instance; the REDMINE_URL env var is the fallback (stdio/single-tenant).
    Returns (base_url_normalized_to_end_with_slash, error_message)."""
    headers = {} if ctx is None or ctx.headers is None else ctx.headers  # no headers on stdio
    override = (headers.get('x-redmine-url') or '').strip().rstrip('/')
    base_raw = override or (REDMINE_URL.rstrip('/') if REDMINE_URL else '')
    if not base_raw:
        return '', "No Redmine URL: set REDMINE_URL or send an X-Redmine-URL request header"
    if not base_raw.lower().startswith(('http://', 'https://')):
        return '', ("Redmine URL is missing an 'http://' or 'https://' protocol: "
                    f"got {base_raw!r}; send X-Redmine-URL with a full URL")
    return base_raw + '/', ''


def resolve_read_only(ctx: Context = None) -> bool:
    """Read-only can be enforced per request via X-Redmine-Read-Only: 1, in addition
    to the server-wide REDMINE_READ_ONLY env switch."""
    if REDMINE_READ_ONLY:
        return True
    if ctx is None or ctx.headers is None:
        return False
    return ctx.headers.get('x-redmine-read-only') == '1'


if REDMINE_DANGEROUSLY_ACCEPT_INVALID_CERTS:
    _ssl_verify = False
elif REDMINE_CA_BUNDLE:
    _ssl_verify = REDMINE_CA_BUNDLE
else:
    _ssl_verify = True

# Persistent HTTP client — reuses TCP/TLS connections across calls instead of opening a new one each time.
# Using httpx.request() (top-level function) creates a new connection per call, which adds ~2 minutes of
# TLS handshake overhead on each request when connecting to internal/corporate Redmine servers.
# keepalive_expiry=120 keeps connections alive for 2 minutes; the httpx default of 5s means every call
# after a short pause pays a full ~600ms TLS reconnect cost.
_http_client = httpx.Client(
    timeout=60.0,
    verify=_ssl_verify,
    limits=httpx.Limits(max_keepalive_connections=5, keepalive_expiry=120),
)

if "REDMINE_REQUEST_INSTRUCTIONS" in os.environ:
    with open(os.environ["REDMINE_REQUEST_INSTRUCTIONS"]) as f:
        REDMINE_REQUEST_INSTRUCTIONS = f.read()
else:
    REDMINE_REQUEST_INSTRUCTIONS = ""


# Core
def request(path: str, method: str = 'get', data: dict = None, params: dict = None,
            content_type: str = 'application/json', content: bytes = None, raw: bool = False,
            ctx: Context = None) -> dict:
    if resolve_read_only(ctx) and method.lower() != 'get':
        return {"status_code": 0, "body": None,
                "error": f"Read-only mode is enabled: refusing {method.upper()} request"}

    # On HTTP transports a client can send its own key per request, so a shared server acts
    # as the calling user instead of one service account (#49). Env key is the fallback.
    headers = {} if ctx is None or ctx.headers is None else ctx.headers  # no headers on stdio
    key = headers.get('x-redmine-api-key') or REDMINE_API_KEY
    if not key:
        return {"status_code": 0, "body": None,
                "error": "No API key: set REDMINE_API_KEY or send an X-Redmine-API-Key request header"}

    # Same per-request override for the Redmine base URL (X-Redmine-URL header).
    redmine_url, url_error = resolve_redmine_url(ctx)
    if url_error:
        return {"status_code": 0, "body": None, "error": url_error}

    headers = {
        'X-Redmine-API-Key': key,
        'Content-Type': content_type,
        **REDMINE_HEADERS
    }

    # Security: path is model-controlled. urljoin returns absolute URLs in `path` unchanged, which would
    # redirect the request (and the API key header) to an arbitrary host. Only ever allow URLs that stay
    # under the resolved base URL (normalized to end with '/'), also catching '../' path traversal.
    url = urljoin(redmine_url, path.lstrip('/'))
    if not url.startswith(redmine_url):
        return {"status_code": 0, "body": None,
                "error": f"Path escapes Redmine base URL, refusing to send API key to: {url}"}

    try:
        response = _http_client.request(method=method.lower(), url=url, json=data, params=params, headers=headers,
                                       content=content)
        response.raise_for_status()

        body = None
        if raw:
            # Downloads must keep the exact bytes: an attachment that happens to be valid JSON
            # (e.g. a Postman collection) must not be parsed into a dict (#46).
            body = response.content
        elif response.content:
            try:
                body = response.json()
            except ValueError:
                body = response.content

        return {"status_code": response.status_code, "body": body, "error": ""}
    except Exception as e:
        try:
            status_code = e.response.status_code
        except:
            status_code = 0

        try:
            body = e.response.json()
        except:
            try:
                body = e.response.text
            except:
                body = None

        return {"status_code": status_code, "body": body, "error": f"{e.__class__.__name__}: {e}"}
        
def redact(obj):
    """Redact credential values from parsed API responses. Redmine returns the requesting
    user's api_key on several /users endpoints, and tool results end up in model context,
    transcripts and logs (reported in #50, thanks @nick411077)."""
    if isinstance(obj, dict):
        return {k: ("[REDACTED]" if k.lower().replace("-", "_") in ("api_key", "apikey") else redact(v))
                for k, v in obj.items()}
    if isinstance(obj, list):
        return [redact(x) for x in obj]
    return obj


def scrub_key(text: str) -> str:
    """Remove the configured API key from any outgoing text, wherever it appears."""
    if REDMINE_API_KEY and isinstance(text, str):
        return text.replace(REDMINE_API_KEY, "[REDACTED]")
    return text


def format_response(obj):
    """Format response as YAML or JSON based on REDMINE_RESPONSE_FORMAT env var."""
    obj = redact(obj)
    if REDMINE_RESPONSE_FORMAT == 'json':
        return scrub_key(json.dumps(obj, ensure_ascii=False, indent=2, default=str))
    # YAML: Allow direct Unicode output, prevent line wrapping for long lines, and avoid automatic key sorting.
    return scrub_key(yaml.safe_dump(obj, allow_unicode=True, sort_keys=False, width=4096))



def wrap_insecure_content(content: str) -> str:
    """Wrap content that may contain user-generated data with security tags to prevent prompt injection."""
    tag_id = uuid.uuid4().hex[:16]
    return f"<insecure-content-{tag_id}>\n{content}\n</insecure-content-{tag_id}>"


def validate_path(file_path: str, must_exist: bool = True) -> tuple[str | None, pathlib.Path | None]:
    """
    Validate and resolve a file path.
    Returns (None, resolved_path) on success, (error_message, None) on failure.
    """
    # Require allowed directories to be configured (secure by default)
    if not REDMINE_ALLOWED_DIRECTORIES:
        return "File operations disabled: REDMINE_ALLOWED_DIRECTORIES not configured", None

    try:
        path = pathlib.Path(file_path).expanduser().resolve()
    except Exception as e:
        return f"Invalid path: {file_path} ({e})", None

    if not path.is_absolute():
        return f"Path must be absolute, got: {file_path}", None

    # Check path is within allowed directories
    if not any(path.is_relative_to(allowed) for allowed in REDMINE_ALLOWED_DIRECTORIES):
        return (f"Path not in allowed directories: {file_path}. This server only sees files inside "
                "REDMINE_ALLOWED_DIRECTORIES on its own filesystem; paths inside another container or "
                "sandbox (e.g. /workspace/...) are not reachable here. Upload such content via "
                "redmine_upload_content instead, passing the file bytes as base64."), None

    if must_exist and not path.exists():
        return f"File not found: {path}", None

    return None, path


# Tools
mcp = MCPServer("Redmine MCP server", version=VERSION)
get_logger(__name__).info(f"Starting MCP Redmine version {VERSION}")

@mcp.tool(description="""
Make a request to the Redmine API

Args:
    path: API endpoint path (e.g. '/issues.json')
    method: HTTP method to use (default: 'get')
    data: Dictionary for request body (for POST/PUT)
    params: Dictionary for query parameters

Returns:
    str: YAML string containing response status code, body and error message

{}""".format(REDMINE_REQUEST_INSTRUCTIONS).strip())
    
def redmine_request(path: str, method: str = 'get', data: dict = None, params: dict = None,
                    ctx: Context = None) -> str:
    return wrap_insecure_content(format_response(
        request(path, method=method, data=data, params=params, ctx=ctx)))

@mcp.tool()
def redmine_paths_list() -> str:
    """Return a list of available API paths from OpenAPI spec
    
    Retrieves all endpoint paths defined in the Redmine OpenAPI specification. Remember that you can use the
    redmine_paths_info tool to get the full specfication for a path.
    
    Returns:
        str: YAML string containing a list of path templates (e.g. '/issues.json')
    """
    return format_response(list(SPEC['paths'].keys()))

@mcp.tool()
def redmine_paths_info(path_templates: list) -> str:
    """Get full path information for given path templates
    
    Args:
        path_templates: List of path templates (e.g. ['/issues.json', '/projects.json'])
        
    Returns:
        str: YAML string containing API specifications for the requested paths
    """
    info = {}
    for path in path_templates:
        if path in SPEC['paths']:
            info[path] = SPEC['paths'][path]

    return format_response(info)

@mcp.tool()
def redmine_upload(file_path: str, description: str = None, ctx: Context = None) -> str:
    """
    Upload a file to Redmine and get a token for attachment

    Only works for files that already exist on THIS server's filesystem inside
    REDMINE_ALLOWED_DIRECTORIES. Paths belonging to a caller's own container or
    sandbox (for example chat attachments projected under /workspace) are not
    visible here and will be rejected. Upload those by sending their bytes to
    redmine_upload_content (base64) instead.

    Args:
        file_path: Fully qualified path to the file to upload (must be within REDMINE_ALLOWED_DIRECTORIES)
        description: Optional description for the file

    Returns:
        str: YAML string containing response status code, body and error message
             The body contains the attachment token
    """
    error, path = validate_path(file_path, must_exist=True)
    if error:
        return format_response({"status_code": 0, "body": None, "error": error})

    try:
        params = {'filename': path.name}
        if description:
            params['description'] = description

        with open(path, 'rb') as f:
            file_content = f.read()

        result = request(path='uploads.json', method='post', params=params,
                         content_type='application/octet-stream', content=file_content,
                         ctx=ctx)
        return format_response(result)
    except Exception as e:
        return format_response({"status_code": 0, "body": None, "error": f"{e.__class__.__name__}: {e}"})

@mcp.tool()
def redmine_upload_content(filename: str, content: str, content_type: str = None,
                           description: str = None, issue_id: int = None,
                           ctx: Context = None) -> str:
    """
    Upload in-memory file content to Redmine and optionally attach it to an issue.

    Use this instead of redmine_upload when the caller already holds the file
    bytes (e.g. a chat attachment read by the Go service) and has no shared
    filesystem with this server. When issue_id is provided, this tool performs
    the upload and issue update in one operation; otherwise the returned token
    is not attached to anything yet and must be linked with redmine_request.

    Args:
        filename: The file name Redmine stores (e.g. 'screenshot.png').
        content: Base64-encoded file bytes (standard alphabet, whitespace tolerated).
        content_type: MIME type of the file, e.g. 'image/png', 'application/pdf'.
            Defaults to 'application/octet-stream'.
        description: Optional description for the file.
        issue_id: Optional Redmine issue ID to attach the upload to.

    Returns:
        str: YAML string containing response status code, body and error message.
             On success body.upload.token is the attachment token. When issue_id
             is supplied, body.issue_id confirms the linked issue.
    """
    log = get_logger(__name__)
    filename = (filename or '').strip()
    if not filename:
        return format_response({"status_code": 0, "body": None, "error": "filename is required"})
    if not content:
        return format_response({"status_code": 0, "body": None, "error": "content is required"})
    if issue_id is not None and (
        isinstance(issue_id, bool) or not isinstance(issue_id, int) or issue_id <= 0
    ):
        return format_response({"status_code": 0, "body": None,
                                "error": "issue_id must be a positive integer"})

    # Reject before decoding so an oversized argument never allocates the raw
    # buffer. Base64 encodes 3 bytes per 4 characters; allow encoder padding.
    compact = ''.join(content.split())
    if len(compact) > (REDMINE_UPLOAD_MAX_BYTES + 2) // 3 * 4 + 4:
        return format_response({"status_code": 0, "body": None,
                                "error": f"File too large: decoded content would exceed the "
                                         f"{REDMINE_UPLOAD_MAX_BYTES}-byte upload limit"})
    try:
        raw = base64.b64decode(compact, validate=True)
    except Exception:
        # Generic message on purpose: never echo payload fragments back.
        return format_response({"status_code": 0, "body": None,
                                "error": "content is not valid base64-encoded data"})
    if len(raw) == 0:
        return format_response({"status_code": 0, "body": None, "error": "decoded content is empty"})
    if len(raw) > REDMINE_UPLOAD_MAX_BYTES:
        return format_response({"status_code": 0, "body": None,
                                "error": f"File too large: {len(raw)} bytes "
                                         f"(max {REDMINE_UPLOAD_MAX_BYTES})"})

    mime = (content_type or '').strip() or 'application/octet-stream'
    params = {'filename': filename}
    if description:
        params['description'] = description

    # Metadata only: the base64 payload and decoded bytes must never be logged.
    log.info("redmine_upload_content: filename=%s size=%d content_type=%s",
             filename, len(raw), mime)

    result = request(path='uploads.json', method='post', params=params,
                     content_type='application/octet-stream', content=raw, ctx=ctx)
    if isinstance(result.get("body"), dict) and 200 <= result.get("status_code", 0) < 300:
        upload = result["body"].setdefault("upload", {})
        if isinstance(upload, dict):
            upload.setdefault("filename", filename)
            upload["content_type"] = mime
            token = upload.get("token")
            if issue_id is not None and token:
                link_result = request(
                    path=f'issues/{issue_id}.json',
                    method='put',
                    data={'issue': {'uploads': [{
                        'token': token,
                        'filename': filename,
                        'content_type': mime,
                    }]}},
                    ctx=ctx,
                )
                if 200 <= link_result.get("status_code", 0) < 300:
                    result["body"]["issue_id"] = issue_id
                else:
                    result["status_code"] = link_result.get("status_code", 0)
                    result["error"] = (
                        f"Upload succeeded, but linking the attachment to issue "
                        f"{issue_id} failed: {link_result.get('error', '')}"
                    )
    return format_response(result)

@mcp.tool()
def redmine_download(attachment_id: int, save_path: str, filename: str | None = None,
                     ctx: Context = None) -> str:
    """
    Download an attachment from Redmine and save it to a local file

    Args:
        attachment_id: The ID of the attachment to download
        save_path: Fully qualified file path (not directory) where the file should be saved to (must be within REDMINE_ALLOWED_DIRECTORIES)
        filename: Optional filename for the Redmine download URL. If not provided,
                 will be determined from attachment metadata. Does not affect the local save path.

    Returns:
        str: YAML string containing download status, file path, and any error messages
    """
    error, path = validate_path(save_path, must_exist=False)
    if error:
        return format_response({"status_code": 0, "body": None, "error": error})

    if path.is_dir():
        return format_response({"status_code": 0, "body": None, "error": f"Path can't be a directory: {save_path}"})

    try:
        if not filename:
            attachment_response = request(f"attachments/{attachment_id}.json", "get",
                                          ctx=ctx)
            if attachment_response["status_code"] != 200:
                return format_response(attachment_response)

            filename = attachment_response["body"]["attachment"]["filename"]

        response = request(f"attachments/download/{attachment_id}/{filename}", "get",
                           content_type="application/octet-stream", raw=True, ctx=ctx)
        if response["status_code"] != 200 or not response["body"]:
            return format_response(response)

        # Create parent directories if needed
        path.parent.mkdir(parents=True, exist_ok=True)

        with open(path, 'wb') as f:
            f.write(response["body"])

        return format_response({"status_code": 200, "body": {"saved_to": str(path), "filename": filename}, "error": ""})
    except Exception as e:
        return format_response({"status_code": 0, "body": None, "error": f"{e.__class__.__name__}: {e}"})

# Max size for images returned inline as tool content (base64 roughly x1.33, so keep this modest)
ATTACHMENT_IMAGE_MAX_BYTES = 5 * 1024 * 1024

@mcp.tool()
def redmine_attachment_image(attachment_id: int, ctx: Context = None) -> Image | str:
    """
    Fetch an image attachment (e.g. an inline screenshot like !screenshot.png!) and return it as viewable
    image content. Use redmine_request on '/issues/{id}.json' with params {'include': 'attachments'} to find
    attachment ids.

    Args:
        attachment_id: The ID of the image attachment to fetch

    Returns:
        Image content on success, or a YAML error string on failure
    """
    try:
        attachment_response = request(f"attachments/{attachment_id}.json", "get",
                                      ctx=ctx)
        if attachment_response["status_code"] != 200:
            return format_response(attachment_response)

        attachment = attachment_response["body"]["attachment"]
        content_type = attachment.get("content_type") or ""
        if not content_type.startswith("image/"):
            return format_response({
                "status_code": 0, "body": None,
                "error": f"Attachment is not an image (content_type: {content_type}). "
                         "Use redmine_download to save it to disk instead."})
        if attachment.get("filesize", 0) > ATTACHMENT_IMAGE_MAX_BYTES:
            return format_response({
                "status_code": 0, "body": None,
                "error": f"Image too large ({attachment['filesize']} bytes, max {ATTACHMENT_IMAGE_MAX_BYTES}). "
                         "Use redmine_download to save it to disk instead."})

        response = request(f"attachments/download/{attachment_id}/{attachment['filename']}", "get",
                           content_type="application/octet-stream", raw=True, ctx=ctx)
        if response["status_code"] != 200 or not response["body"]:
            return format_response(response)

        return Image(data=response["body"], format=content_type.removeprefix("image/"))
    except Exception as e:
        return format_response({"status_code": 0, "body": None, "error": f"{e.__class__.__name__}: {e}"})

@mcp.tool()
def redmine_get_issue(issue_id: int, include_images: bool = True, ctx: Context = None) -> list[Image | str] | str:
    """Fetch a Redmine issue (bug) by ID with full details and inline its image attachments
    as viewable images, so screenshots referenced in the description or comments can be seen
    directly instead of only as metadata.

    The issue is fetched with attachments and journals (history/comments). Image attachments
    (content_type image/*) up to the inline size limit are returned as viewable image content
    after the issue text; non-image or larger attachments stay as metadata — use redmine_download
    to save those to disk.

    Args:
        issue_id: The Redmine issue ID.
        include_images: When True (default), fetch and return image attachments inline as
            viewable images. Set to False for text-only details (faster on issues with many
            or large images).

    Returns:
        A list of content items: the issue details as YAML text, then each image attachment
        as image content. On error, a YAML error string.
    """
    try:
        issue_response = request(f"issues/{issue_id}.json", "get",
                                 params={"include": "attachments,journals"}, ctx=ctx)
        if issue_response["status_code"] != 200 or not issue_response["body"]:
            return wrap_insecure_content(format_response(issue_response))

        body = issue_response["body"]
        issue = body.get("issue", body)
        attachments = issue.get("attachments") or []

        inline, skipped = [], []
        for a in attachments:
            content_type = a.get("content_type") or ""
            if content_type.startswith("image/") and (a.get("filesize") or 0) <= ATTACHMENT_IMAGE_MAX_BYTES:
                inline.append(a)
            else:
                skipped.append(a)

        # Annotate the summary so the model knows which images follow inline and which don't.
        summary = dict(body)
        summary["_inline_images"] = [{"id": a.get("id"), "filename": a.get("filename"),
                                      "content_type": a.get("content_type"),
                                      "filesize": a.get("filesize")} for a in inline]
        if skipped:
            summary["_non_inline_attachments"] = [{"id": a.get("id"), "filename": a.get("filename"),
                                                   "content_type": a.get("content_type"),
                                                   "filesize": a.get("filesize")} for a in skipped]

        text = wrap_insecure_content(format_response(summary))

        if not include_images or not inline:
            return [text]

        content: list[Image | str] = [text]
        for a in inline:
            dl = request(f"attachments/download/{a['id']}/{a['filename']}", "get",
                         content_type="application/octet-stream", raw=True, ctx=ctx)
            if dl["status_code"] == 200 and dl["body"]:
                ct = a.get("content_type") or "image/png"
                content.append(Image(data=dl["body"], format=ct.removeprefix("image/")))
            else:
                content.append(f"[Failed to fetch image attachment id={a.get('id')} "
                               f"({a.get('filename')}): {dl.get('error') or dl.get('status_code')}]")
        return content
    except Exception as e:
        return format_response({"status_code": 0, "body": None, "error": f"{e.__class__.__name__}: {e}"})

# Reject unknown tool arguments. The SDK's generated argument models default to ignoring
# extras, so a model that writes body= instead of data= would send an empty request and
# get a 204 back as if it worked (#52). Forbidding extras also makes the published schema
# say additionalProperties: false, so validating clients reject the typo too.
# The SDK has no option for this, hence the private attributes. mcp is pinned to an exact
# version, so an SDK rename fails at import and gets caught by the tests before a release.
for _tool in mcp._tool_manager.list_tools():
    _tool.fn_metadata.arg_model.model_config["extra"] = "forbid"
    _tool.fn_metadata.arg_model.model_rebuild(force=True)
    _tool.parameters["additionalProperties"] = False  # the schema is cached at registration


def main():
    """Main entry point for the mcp-redmine package."""
    import argparse
    parser = argparse.ArgumentParser(description="MCP Redmine Server")
    parser.add_argument("--transport", choices=["stdio", "streamable-http", "sse"], default="stdio",
                        help="Transport type (default: stdio). streamable-http is the recommended HTTP "
                             "transport, sse is supported for legacy clients.")
    parser.add_argument("--host", default="0.0.0.0", help="Host for HTTP transports (default: 0.0.0.0)")
    parser.add_argument("--port", type=int, default=8000, help="Port for HTTP transports (default: 8000)")
    args = parser.parse_args()

    if args.transport == "stdio":
        mcp.run(transport="stdio")
    else:
        mcp.run(transport=args.transport, host=args.host, port=args.port)

if __name__ == "__main__":
    main()
