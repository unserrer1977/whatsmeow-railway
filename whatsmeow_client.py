"""
WhatsApp API Client SDK for whatsmeow Railway service.

Usage:
    from whatsmeow_client import WhatsAppClient

    client = WhatsAppClient(
        base_url="https://whatsmeow-api-production-28ac.up.railway.app",
        api_key="your-secret-api-key"
    )

    # Send a private message
    client.send_message(phone="1234567890", message="Hello!")

    # List groups
    groups = client.list_groups()

    # Send a group message
    client.send_group_message(group_id="120363xxx@g.us", message="Hello group!")

    # Get status
    status = client.status()
"""

import json
import requests
from typing import Optional, List, Dict, Any


class WhatsAppClient:
    """Programmatic client for the whatsmeow WhatsApp API service."""

    def __init__(self, base_url: str, api_key: Optional[str] = None, timeout: int = 30):
        """
        Initialize the WhatsApp API client.

        Args:
            base_url: The base URL of the deployed whatsmeow service.
            api_key: API key for authenticated endpoints. Set API_KEY env var
                     on the service to require this.
            timeout: Request timeout in seconds.
        """
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout
        self.session = requests.Session()
        if api_key:
            self.session.headers["X-API-Key"] = api_key
        self.session.headers["Content-Type"] = "application/json"

    # ─── Public endpoints ───────────────────────────────────────────────

    def health(self) -> Dict[str, Any]:
        """Check if the service is alive."""
        r = self.session.get(f"{self.base_url}/health", timeout=self.timeout)
        r.raise_for_status()
        return r.json()

    def status(self) -> Dict[str, Any]:
        """Get WhatsApp connection status and paired phone number."""
        r = self.session.get(f"{self.base_url}/status", timeout=self.timeout)
        r.raise_for_status()
        return r.json()

    def get_qr(self) -> Dict[str, Any]:
        """Get the current QR code for pairing (if not yet connected)."""
        r = self.session.get(f"{self.base_url}/qr", timeout=self.timeout)
        r.raise_for_status()
        return r.json()

    # ─── Messaging ──────────────────────────────────────────────────────

    def send_message(self, phone: str, message: str) -> Dict[str, Any]:
        """
        Send a text message to a private chat (individual contact).

        Args:
            phone: Phone number in E.164 format (country code + number, no + or spaces).
                   e.g. "1234567890" for US, "447123456789" for UK.
            message: Text message content.

        Returns:
            Dict with 'success' (bool) and 'messageId' (str) on success,
            or 'error' (str) on failure.
        """
        payload = {"phone": phone, "message": message}
        r = self.session.post(
            f"{self.base_url}/api/send",
            json=payload,
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    def send_group_message(self, group_id: str, message: str) -> Dict[str, Any]:
        """
        Send a text message to a WhatsApp group.

        Args:
            group_id: The group JID (e.g. "120363xxxxxxxxxx@g.us").
                      Use list_groups() to find group IDs.
            message: Text message content.

        Returns:
            Dict with 'success' (bool) and 'messageId' (str) on success.
        """
        payload = {"groupId": group_id, "message": message}
        r = self.session.post(
            f"{self.base_url}/api/send-group",
            json=payload,
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    def send(self, message: str, phone: Optional[str] = None,
             group_id: Optional[str] = None) -> Dict[str, Any]:
        """
        Unified send method — specify either phone or group_id.

        Args:
            message: Text message content.
            phone: Phone number for private chat (optional).
            group_id: Group JID for group message (optional).

        Returns:
            Dict with 'success' and 'messageId'.
        """
        if not phone and not group_id:
            raise ValueError("Either phone or group_id must be provided")
        payload = {"message": message}
        if phone:
            payload["phone"] = phone
        if group_id:
            payload["groupId"] = group_id
        r = self.session.post(
            f"{self.base_url}/api/send",
            json=payload,
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    def revoke_message(self, message_id: str,
                       phone: Optional[str] = None,
                       group_id: Optional[str] = None) -> Dict[str, Any]:
        """
        Revoke (unsend) a previously sent message.

        Args:
            message_id: The message ID returned from send_message/send_group_message.
            phone: Phone number of the chat (for private messages).
            group_id: Group JID (for group messages).

        Returns:
            Dict with 'success' (bool).
        """
        payload = {"messageId": message_id}
        if phone:
            payload["phone"] = phone
        if group_id:
            payload["groupId"] = group_id
        r = self.session.post(
            f"{self.base_url}/api/revoke",
            json=payload,
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    # ─── Groups ─────────────────────────────────────────────────────────

    def list_groups(self) -> List[Dict[str, Any]]:
        """
        List all WhatsApp groups the account has joined.

        Returns:
            List of dicts with 'jid', 'name', 'owner', 'participantCount',
            'isAnnounce', 'isLocked'.
        """
        r = self.session.get(f"{self.base_url}/api/groups", timeout=self.timeout)
        r.raise_for_status()
        return r.json().get("groups", [])

    def get_group_info(self, group_jid: str) -> Dict[str, Any]:
        """
        Get detailed info about a specific group.

        Args:
            group_jid: The group JID (e.g. "120363xxx@g.us").

        Returns:
            Dict with group details.
        """
        r = self.session.get(
            f"{self.base_url}/api/group/{group_jid}",
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    # ─── Contacts ───────────────────────────────────────────────────────

    def list_contacts(self) -> List[Dict[str, Any]]:
        """
        List known contacts (synced from the paired WhatsApp account).

        Returns:
            List of dicts with 'jid' and 'name'.
        """
        r = self.session.get(f"{self.base_url}/api/contacts", timeout=self.timeout)
        r.raise_for_status()
        return r.json().get("contacts", [])

    # ─── Convenience ────────────────────────────────────────────────────

    def is_connected(self) -> bool:
        """Check if WhatsApp is currently connected."""
        return self.status().get("connected", False)

    def get_phone(self) -> Optional[str]:
        """Get the paired phone number."""
        return self.status().get("phone")

    def wait_until_connected(self, timeout: int = 120, interval: int = 5) -> bool:
        """
        Block until WhatsApp is connected, or timeout.

        Args:
            timeout: Max seconds to wait.
            interval: Polling interval in seconds.

        Returns:
            True if connected, False if timed out.
        """
        import time
        start = time.time()
        while time.time() - start < timeout:
            if self.is_connected():
                return True
            time.sleep(interval)
        return False

    def __repr__(self) -> str:
        connected = "?"
        try:
            connected = self.is_connected()
        except Exception:
            pass
        return f"WhatsAppClient(url={self.base_url!r}, connected={connected})"
