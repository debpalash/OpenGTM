
import csv
import concurrent.futures
import argparse
import sys
from email_validator import validate_email, EmailNotValidError

MAX_WORKERS = 50

def check_email(row):
    email = row.get('Email', '').strip()
    if not email:
        return None
    
    try:
        v = validate_email(email, check_deliverability=True)
        row['Email'] = v.email
        return row
    except EmailNotValidError:
        return None
    except Exception:
        return None

def validate_emails(input_file, output_file):
    print(f"Reading from {input_file}...")
    try:
        with open(input_file, 'r', encoding='utf-8') as f:
            reader = csv.DictReader(f)
            rows = list(reader)
    except FileNotFoundError:
        print(f"Error: File {input_file} not found.")
        raise FileNotFoundError(f"File {input_file} not found.")

    print(f"Validating {len(rows)} emails using {MAX_WORKERS} workers...")
    
    valid_rows = []
    processed = 0
    total = len(rows)
    
    with concurrent.futures.ThreadPoolExecutor(max_workers=MAX_WORKERS) as executor:
        future_to_email = {executor.submit(check_email, row): row for row in rows}
        
        for future in concurrent.futures.as_completed(future_to_email):
            result = future.result()
            if result:
                valid_rows.append(result)
            
            processed += 1
            if processed % 100 == 0:
                print(f"Processed {processed}/{total}...", end='\r')

    print(f"\nProcessing complete.")
    print(f"Total input: {total}")
    print(f"Valid emails: {len(valid_rows)}")
    
    with open(output_file, 'w', newline='', encoding='utf-8') as f:
        fieldnames = ['Name', 'Email']
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(valid_rows)
        
    print(f"Valid emails written to {output_file}")

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Validate emails in CSV")
    parser.add_argument("input_file", help="Input CSV file path")
    parser.add_argument("output_file", help="Output CSV file path")
    args = parser.parse_args()

    validate_emails(args.input_file, args.output_file)
