import sys
import os
import glob
from sqlalchemy.orm import Session
from datetime import datetime

# Add project root to path
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../")))

from apps.api.database import SessionLocal
from apps.api.models import EmailData
from apps.api.format_emails import format_emails

DATA_DIR = os.path.join(os.path.dirname(__file__), "../../../data")


def backfill():
    print("Starting backfill process...")

    db = SessionLocal()
    try:
        # 1. Load existing emails to avoid duplicates
        # Assuming email is the uniqueness constraint we care about for "whats not added"
        # Note: Ideally we'd have a unique index on email, but user said "add whats not added"
        print("Loading existing emails from DB...")
        existing_emails = set(email for (email,) in db.query(EmailData.email).all())
        print(f"Loaded {len(existing_emails)} existing emails.")

        # 2. Find all .txt files
        # Check both data/ and data/scribd/ (as scribdl might save in either depending on config)
        # Based on LS output, they are in 'data/'
        files = glob.glob(os.path.join(DATA_DIR, "**/*.txt"), recursive=True)
        print(f"Found {len(files)} text files to scan.")

        new_records = []
        for file_path in files:
            # Parse
            extracted = format_emails(file_path)
            if not extracted:
                continue

            for item in extracted:
                email = item["Email"]
                if email not in existing_emails:
                    new_records.append(
                        EmailData(
                            name=item["Name"],
                            email=email,
                            source_link_id=None,  # Orphaned / Backfilled
                            created_at=datetime.utcnow().isoformat(),
                        )
                    )
                    existing_emails.add(
                        email
                    )  # Prevent duplicates processing multiple files

        # 3. Bulk Insert
        if new_records:
            print(f"Found {len(new_records)} NEW records.")
            print("Inserting into database...")
            # Batch insert in chunks of 5000 to avoid packet size issues
            BATCH_SIZE = 5000
            for i in range(0, len(new_records), BATCH_SIZE):
                batch = new_records[i : i + BATCH_SIZE]
                db.bulk_save_objects(batch)
                db.commit()  # Commit each batch
                print(f"Inserted batch {i} - {i + len(batch)}")

            print("Backfill complete.")
        else:
            print("No new records found.")

    except Exception as e:
        print(f"Error during backfill: {e}")
    finally:
        db.close()


if __name__ == "__main__":
    backfill()
