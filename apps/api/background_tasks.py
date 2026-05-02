async def process_direct_url_task(url: str, link_id: int):
    """
    Background task to process a direct URL.
    """
    from apps.api.url_processor import url_processor
    from apps.api.api import SessionLocal, Link, EmailData
    import asyncio

    db = SessionLocal()
    try:
        # Run synchronous processing in threadpool
        loop = asyncio.get_running_loop()
        result = await loop.run_in_executor(None, url_processor.process_url, url)

        link = db.query(Link).filter(Link.id == link_id).first()
        if not link:
            db.close()
            return

        if result["status"] == "error":
            link.status = "Failed"
            db.commit()
            db.close()
            return

        # Save Results
        entries = result.get("entries", [])
        saved_count = 0

        for entry in entries:
            # Validate email before insertion
            from apps.api.validation_utils import validate_and_normalize_email

            normalized_email = validate_and_normalize_email(entry.get("email", ""))
            if not normalized_email:
                # Skip invalid emails silently
                continue

            # Check if already exists
            exists = (
                db.query(EmailData)
                .filter(
                    EmailData.email == normalized_email,
                    EmailData.source_link_id == link.id,
                )
                .first()
            )

            if not exists:
                new_email = EmailData(
                    name=entry.get("name", ""),
                    email=normalized_email,  # Use validated/normalized email
                    source_link_id=link.id,
                )
                db.add(new_email)
                saved_count += 1

        db.commit()

        link.status = "Completed"
        db.commit()

    except Exception as e:
        print(f"Error processing URL background task: {e}")
        link = db.query(Link).filter(Link.id == link_id).first()
        if link:
            link.status = "Failed"
            db.commit()
    finally:
        db.close()
