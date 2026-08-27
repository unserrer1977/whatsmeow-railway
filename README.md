# whatsmeow Railway service

A WhatsApp web multidevice API service built on [whatsmeow](https://github.com/tulir/whatsmeow), deployed on Railway with a full programmatic interface.

## Quick start

```python
from whatsmeow_client import WhatsAppClient

client = WhatsAppClient(
    base_url="https://whatsmeow-api-production-28ac.up.railway.app",
    api_key="YOUR_API_KEY"
)

# Send a message to an individual
client.send_message(phone="1234567890", message="Hello from my app!")

# List groups
groups = client.list_groups()
for g in groups:
    print(f"{g['name']} — {g['jid']} ({g['participantCount']} members)")

# Send a group message
client.send_group_message(group_id=groups[0]['jid'], message="Hello group!")

# Revoke (unsend) a message
result = client.send_message(phone="1234567890", message="Oops")
client.revoke_message(message_id=result['messageId'], phone="1234567890")
```

## API reference

### Public endpoints (no auth)

| Endpoint | Method | Description |
|---|---|---|
| `/health` | GET | Health check |
| `/qr` | GET | Current QR code for pairing |
| `/status` | GET | Connection status + paired phone |

### Authenticated endpoints (require `X-API-Key` header)

| Endpoint | Method | Description |
|---|---|---|
| `/api/send` | POST | Send a message (private or group) |
| `/api/send-group` | POST | Send a message to a group |
| `/api/groups` | GET | List all joined groups |
| `/api/group/{jid}` | GET | Get info about a specific group |
| `/api/contacts` | GET | List known contacts |
| `/api/revoke` | POST | Revoke (unsend) a message |

### Authentication

All `/api/*` endpoints require an API key. Set the `API_KEY` environment variable on the Railway service, then pass it in requests:

```
X-API-Key: your-secret-key
```

Or as a Bearer token:
```
Authorization: Bearer your-secret-key
```

### Send a message

```bash
# Private chat
curl -X POST https://your-service.up.railway.app/api/send \
  -H "X-API-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"phone":"1234567890","message":"Hello!"}'

# Group chat
curl -X POST https://your-service.up.railway.app/api/send \
  -H "X-API-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"groupId":"120363xxx@g.us","message":"Hello group!"}'
```

Response:
```json
{"success": true, "messageId": "3EB0XXXXX"}
```

### List groups

```bash
curl -H "X-API-Key: YOUR_KEY" \
  https://your-service.up.railway.app/api/groups
```

Response:
```json
{
  "groups": [
    {
      "jid": "120363xxx@g.us",
      "name": "My Group",
      "owner": "1234567890@s.whatsapp.net",
      "participantCount": 15,
      "isAnnounce": false,
      "isLocked": false
    }
  ]
}
```

### Incoming messages

Incoming messages are logged as JSON to the Railway deploy logs:
```
INCOMING_MESSAGE_JSON: {"from":"1234567890@s.whatsapp.net","isGroup":false,"sender":"1234567890@s.whatsapp.net","message":"Hi","timestamp":1234567890,"messageId":"..."}
```

## Python SDK

The `whatsmeow_client.py` file is a drop-in client. Copy it into your project or install alongside your app.

```bash
pip install requests
```

```python
from whatsmeow_client import WhatsAppClient

client = WhatsAppClient(
    base_url="https://whatsmeow-api-production-28ac.up.railway.app",
    api_key="YOUR_API_KEY"
)

# Check connection
print(client.status())

# Send messages
client.send_message(phone="1234567890", message="Hello!")
client.send_group_message(group_id="120363xxx@g.us", message="Hello group!")

# Unified send
client.send(message="Hi", phone="1234567890")
client.send(message="Hi", group_id="120363xxx@g.us")

# Groups
groups = client.list_groups()
info = client.get_group_info("120363xxx@g.us")

# Contacts
contacts = client.list_contacts()

# Revoke
client.revoke_message(message_id="3EB0XXXXX", phone="1234567890")

# Helpers
client.is_connected()
client.get_phone()
client.wait_until_connected(timeout=120)
```

## Session persistence

The WhatsApp session is stored in SQLite at `/data/whatsmeow.db` on a Railway persistent volume. The session survives redeployments, restarts, and crashes. Auto-reconnect is built in.
