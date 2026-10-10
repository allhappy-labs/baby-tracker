# Private MCP secret mount

Docker Compose mounts this directory read-only at `/run/mcp`. It is empty by
default, and the private read-only MCP endpoint is disabled by default, so no
file is needed here. To enable MCP, set `BT_MCP_SECRETS_DIR` to a protected
host directory instead (never commit secrets here) containing the history
cursor key: 64 lowercase hexadecimal characters (32 random bytes), for example
`openssl rand -hex 32 > cursor_key`, readable by server UID 65532 with mode
0600. Then set `BT_MCP_CURSOR_KEY_FILE=/run/mcp/cursor_key`. The MCP database
URL may reuse the runtime role's secret
(`BT_MCP_DATABASE_URL_FILE=/run/secrets/runtime_database_url`); the server
opens it as a separate read-only pool.
