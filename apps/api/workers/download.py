import asyncio
import os
import datetime
from functools import partial
from apps.api.database import SessionLocal
from apps.api.models import Link, EmailData
from apps.api.services.log_stream import manager
from apps.api.scribdl import get_scribd_document
from apps.api.routers.websockets import broadcast_queue_state
from apps.api.format_emails import format_emails
from apps.api.validation_utils import validate_email_batch


async def handle_download_link(job_id: int, payload: dict):
    url = payload.get("url")

    # Fallback: Resolve from link_id
    if not url and payload.get("link_id"):
        db = SessionLocal()
        try:
            link_obj = db.query(Link).filter(Link.id == payload["link_id"]).first()
            if link_obj:
                url = link_obj.url
        finally:
            db.close()

    print(f"Handling download link job {job_id} for {url}")

    link_id = payload.get("link_id")
    if not link_id:
        # Try to find link_id from URL if not provided directly
        db = SessionLocal()
        try:
            link_obj = db.query(Link).filter(Link.url == url).first()
            if link_obj:
                link_id = link_obj.id
        finally:
            db.close()

    def update_link_status(status):
        if link_id:
            db = SessionLocal()
            try:
                link = db.query(Link).filter(Link.id == link_id).first()
                if link:
                    link.status = status
                    if status == "Completed":
                        link.updated_at = datetime.datetime.utcnow().isoformat()
                    db.commit()
            except Exception as e:
                print(f"Failed to update link status: {e}")
            finally:
                db.close()

            # Broadcast update immediately
            asyncio.create_task(broadcast_queue_state())

    if not url:
        if link_id:
            update_link_status("Failed")
        raise ValueError(
            f"No URL provided in payload for Job {job_id}. Payload: {payload}"
        )

    # Set to Processing
    update_link_status("Processing")

    # Define a sync callback adapter
    def status_callback(msg):
        print(f"[Job {job_id}] {msg}")

    loop = asyncio.get_event_loop()

    # Run the blocking download in a thread
    try:
        # 1. Download PDF (images=True)
        output_path = await loop.run_in_executor(
            None,
            partial(
                get_scribd_document, url, images=True, status_callback=status_callback
            ),
        )

        # Notify success via WS
        await manager.broadcast_json(
            {
                "type": "log",
                "message": f"Download finished: {os.path.basename(output_path)}",
                "taskId": link_id or job_id,
            }
        )

        # 2. Trigger Text Extraction & Parsing (images=False)
        try:
            status_callback("Extracting text data...")
            text_path = await loop.run_in_executor(
                None,
                partial(
                    get_scribd_document,
                    url,
                    images=False,  # status_callback can remain same or be None
                ),
            )

            if text_path and os.path.exists(text_path):
                # Parse text
                extracted_data = await loop.run_in_executor(
                    None, format_emails, text_path
                )

                if extracted_data:
                    status_callback(
                        f"Found {len(extracted_data)} records. Validating emails..."
                    )

                    # Validate emails
                    valid_data, stats = validate_email_batch(
                        extracted_data, email_key="Email"
                    )

                    # Send detailed feedback to user
                    validation_msg = (
                        f"Email validation: {stats['valid']} valid, "
                        f"{stats['invalid']} invalid out of {stats['total']} total"
                    )
                    status_callback(validation_msg)

                    await manager.broadcast_json(
                        {
                            "type": "email_extraction",
                            "message": validation_msg,
                            "taskId": link_id or job_id,
                            "stats": stats,
                        }
                    )

                    if valid_data:
                        # Bulk Insert only valid emails
                        db = SessionLocal()
                        try:
                            # Prepare objects with validated emails
                            email_objs = [
                                EmailData(
                                    name=item["Name"],
                                    email=item["Email"],
                                    source_link_id=link_id,
                                    created_at=datetime.datetime.utcnow().isoformat(),
                                )
                                for item in valid_data
                            ]

                            db.bulk_save_objects(email_objs)
                            db.commit()
                            status_callback(
                                f"Saved {len(valid_data)} valid records to database."
                            )

                            # Broadcast success with stats
                            await manager.broadcast_json(
                                {
                                    "type": "log",
                                    "message": f"✓ Saved {len(valid_data)} validated emails to database",
                                    "taskId": link_id or job_id,
                                }
                            )
                        except Exception as e:
                            print(f"DB Insert Error: {e}")
                            status_callback(f"Error saving data: {str(e)}")
                        finally:
                            db.close()
                    else:
                        status_callback("No valid emails found after validation.")
                else:
                    status_callback("No structured data found in text.")

        except Exception as e:
            print(f"Text extraction failed: {e}")
            # Don't fail the whole job if only extraction fails, but log it
            status_callback(f"Warning: Data extraction failed: {str(e)}")

        update_link_status("Completed")

    except Exception as e:
        print(f"Error downloading {url}: {e}")
        update_link_status("Failed")
        await manager.broadcast_json(
            {
                "type": "log",
                "message": f"Download failed for Task #{link_id or job_id}: {str(e)}",
                "taskId": link_id or job_id,
            }
        )
        raise e
