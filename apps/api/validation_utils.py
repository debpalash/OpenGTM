"""
Email validation utility module.
Provides functions to validate and normalize email addresses before database insertion.
"""

import re
from typing import Optional
from email_validator import validate_email, EmailNotValidError


def validate_and_normalize_email(
    email: str, check_deliverability: bool = False
) -> Optional[str]:
    """
    Validates and normalizes an email address.

    Args:
        email: The email address to validate
        check_deliverability: Whether to perform DNS checks (slower but more thorough)

    Returns:
        Normalized email address if valid, None if invalid
    """
    if not email or not isinstance(email, str):
        return None

    # Strip whitespace
    email = email.strip()

    if not email:
        return None

    # Basic regex pattern as first-pass filter
    email_pattern = r"^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$"
    if not re.match(email_pattern, email):
        return None

    try:
        # Use email-validator library for thorough validation
        validated = validate_email(email, check_deliverability=check_deliverability)
        # Return normalized email
        return validated.email
    except EmailNotValidError:
        return None
    except Exception:
        # Catch any other unexpected errors
        return None


def validate_email_batch(
    emails: list[dict], email_key: str = "Email"
) -> tuple[list[dict], dict]:
    """
    Validates a batch of email records and returns statistics.

    Args:
        emails: List of dictionaries containing email data
        email_key: The dictionary key containing the email address

    Returns:
        Tuple of (valid_records, statistics)
        - valid_records: List of records with valid emails
        - statistics: Dict with 'total', 'valid', 'invalid' counts
    """
    valid_records = []
    total = len(emails)
    invalid_count = 0

    for record in emails:
        email = record.get(email_key, "")
        normalized = validate_and_normalize_email(email, check_deliverability=False)

        if normalized:
            # Update record with normalized email
            record[email_key] = normalized
            valid_records.append(record)
        else:
            invalid_count += 1

    stats = {"total": total, "valid": len(valid_records), "invalid": invalid_count}

    return valid_records, stats
