# whatsmeow Railway service

A WhatsApp web multidevice API service built on [whatsmeow](https://github.com/tulir/whatsmeow), deployed on Railway.

## Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/health` | GET | Health check |
| `/qr` | GET | Get current QR code for pairing |
| `/status` | GET | Check WhatsApp connection status |
| `/send` | POST | Send a WhatsApp message |
| `/` | GET | Service info |

## Pairing

1. Deploy the service.
2. Hit `GET /qr` to get a QR code string.
3. Open WhatsApp → Settings → Linked Devices → Link a Device.
4. Scan the QR code (use any QR generator to render the string as an image).
5. `GET /status` will show `connected: true` once paired.

## Sending messages

```bash
curl -X POST https://your-service.up.railway.app/send \
  -H "Content-Type: application/json" \
  -d '{"phone":"1234567890","message":"Hello from Railway!"}'
```

Phone numbers are in E.164 format (country code + number, no `+` or spaces).

## Session persistence

The WhatsApp session is stored in SQLite at `/data/whatsmeow.db`. A Railway volume is mounted at `/data` so the session survives redeployments.
