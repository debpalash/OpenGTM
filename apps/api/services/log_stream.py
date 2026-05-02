from typing import List
from fastapi import WebSocket, WebSocketDisconnect
import json
import asyncio


class ConnectionManager:
    def __init__(self):
        self.active_connections: List[WebSocket] = []

    async def connect(self, websocket: WebSocket):
        await websocket.accept()
        self.active_connections.append(websocket)

    def disconnect(self, websocket: WebSocket):
        if websocket in self.active_connections:
            self.active_connections.remove(websocket)

    async def broadcast_json(self, message: dict):
        # We need to broadcast to all connected clients
        # Use a copy to iterate because disconnect might modify the list
        for connection in self.active_connections[:]:
            try:
                await connection.send_json(message)
            except Exception:
                # If send fails, assume disconnected
                self.disconnect(connection)

    async def emit_log(self, message: str, level: str = "info", source: str = "system"):
        """
        Helper to emit a standard log message to the frontend terminal.
        """
        payload = {
            "type": "terminal_log",
            "data": {
                "message": message,
                "level": level,
                "source": source,
                "timestamp": asyncio.get_event_loop().time(),
            },
        }
        await self.broadcast_json(payload)


# Global Instance
manager = ConnectionManager()
