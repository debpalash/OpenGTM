from fastapi import APIRouter, WebSocket, WebSocketDisconnect, Depends
from sqlalchemy.orm import Session
from apps.api.services.log_stream import manager
from apps.api.database import SessionLocal
from apps.api.models import Link
from apps.api.services.queue_service import queue_service
import datetime
import asyncio

router = APIRouter(tags=["WebSocket"])


async def broadcast_queue_state(target_ws: WebSocket = None):
    """
    Broadcasts the current queue state from DB to all or specific client.
    Limit to most recent 100 tasks.
    """
    db = SessionLocal()
    try:
        links = db.query(Link).order_by(Link.id.desc()).limit(100).all()
        queue_data = [
            {"id": l.id, "url": l.url, "status": l.status, "created_at": l.created_at}
            for l in links
        ]
        message = {"type": "queue_update", "data": queue_data}

        if target_ws:
            await target_ws.send_json(message)
        else:
            await manager.broadcast_json(message)
    finally:
        db.close()


@router.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket):
    await manager.connect(websocket)
    try:
        # Send initial queue state
        await broadcast_queue_state(websocket)

        while True:
            data = await websocket.receive_json()
            action = data.get("action")

            if action == "add_task":
                url = data.get("url")
                if url:
                    db = SessionLocal()
                    existing = db.query(Link).filter(Link.url == url).first()

                    if existing:
                        if existing.status in ["Pending", "Processing"]:
                            db.close()
                            await websocket.send_json(
                                {
                                    "type": "error",
                                    "message": f"Task #{existing.id} is already in queue ({existing.status})",
                                }
                            )
                            continue
                        else:
                            # Restart logic
                            existing.status = "Pending"
                            existing.created_at = datetime.datetime.utcnow().isoformat()
                            db.commit()
                            task_id = existing.id
                            queue_service.add_job(
                                db, "download_link", {"link_id": task_id}
                            )
                            db.close()

                            await manager.broadcast_json(
                                {
                                    "type": "log",
                                    "message": f"Task #{task_id} restarted (was {existing.status})",
                                    "taskId": task_id,
                                }
                            )
                            await broadcast_queue_state()
                            continue

                    new_link = Link(
                        url=url,
                        status="Pending",
                        created_at=datetime.datetime.utcnow().isoformat(),
                    )
                    db.add(new_link)
                    db.commit()
                    db.refresh(new_link)
                    task_id = new_link.id

                    queue_service.add_job(db, "download_link", {"link_id": task_id})
                    db.close()

                    await manager.broadcast_json(
                        {
                            "type": "log",
                            "message": f"Task #{task_id} added by user",
                            "taskId": task_id,
                        }
                    )
                    await broadcast_queue_state()

            elif action == "refresh_queue":
                await broadcast_queue_state(websocket)

            # Handle other actions (stop/delete etc) if moved to WS,
            # but REST is preferred for actions.

    except WebSocketDisconnect:
        manager.disconnect(websocket)
    except Exception as e:
        # logfire.error("WebSocket Error", error=e)
        print(f"WS Error: {e}")
