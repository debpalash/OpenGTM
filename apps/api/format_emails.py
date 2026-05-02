import csv
import re
import argparse


def format_emails(input_file, output_file=None):
    try:
        with open(input_file, "r", encoding="utf-8") as f:
            lines = f.readlines()
    except FileNotFoundError:
        print(f"Error: File {input_file} not found.")
        return []

    data = []
    # Skip header lines if they exist
    start_index = 0
    if len(lines) > 0 and "name" in lines[0].lower():
        start_index = 1
    if len(lines) > 1 and "email" in lines[1].lower():
        start_index = 2

    for line in lines[start_index:]:
        line = line.strip()
        if not line:
            continue

        # Robust extraction using Regex
        # 1. Capture email, specifically stopping before Capital letters if they immediately follow TLD
        # This handles cases like: "example.comName" -> capture "example.com"
        # We look for a standard email pattern
        strict_email_pattern = (
            r"([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-z]{2,}(?:\.[a-z]{2,})?)"
        )

        match = re.search(strict_email_pattern, line)
        if match:
            # Check if the match is followed by a Capital letter (merged name)
            # If so, the regex above (greedy on TLD) might have stopped correctly because TLDs are lowercase
            # But let's be sure.
            email = match.group(1)

            # Double check: if email ends with uppercase, trim it (though regex expects lowercase tld)
            # The pattern \.[a-z]{2,} enforces lowercase TLD, so "comVardhaman" won't match "comVardhaman" as TLD.
            # It will match "com" and stop at "V". unique behavior of regex if properly constrained.

            # Remove email from line to get the name
            # Be careful if "email" is "example.com" and line is "example.comName"
            # replacing "example.com" leaves "Name". Correct.
            rest_of_line = line.replace(email, "").strip()

            # Basic cleanup
            name = " ".join(rest_of_line.split())  # Normalize whitespace

            # Heuristic: Remove common leading numbering (e.g., "1.", "1 ")
            name = re.sub(r"^\d+[\.\s]+", "", name)

            # Remove trailing numbers (IDs/Phones) from Name
            # e.g. "Kumar21113071" -> "Kumar"
            name = re.sub(r"\s*(\d{6,}|\d+)$", "", name).strip()

            data.append({"Name": name, "Email": email})

    if output_file:
        with open(output_file, "w", newline="", encoding="utf-8") as csvfile:
            fieldnames = ["Name", "Email"]
            writer = csv.DictWriter(csvfile, fieldnames=fieldnames)

            writer.writeheader()
            for row in data:
                writer.writerow(row)
        print(f"Converted {len(data)} records from {input_file} to {output_file}")

    return data


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Format email list to CSV")
    parser.add_argument("input_file", help="Input text file path")
    parser.add_argument("output_file", help="Output CSV file path")
    args = parser.parse_args()

    format_emails(args.input_file, args.output_file)
