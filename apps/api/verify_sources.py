
import sys
import os
import json
import logging

# Ensure we can import apps
sys.path.append(os.getcwd())

from apps.api.search_scraper import (
    search_scribd, 
    search_slideshare, 
    search_internet_archive, 
    search_google_books, 
    search_annas_archive
)

# Setup logging
logging.basicConfig(level=logging.INFO, format='%(levelname)s: %(message)s')

def verify_all_sources():
    query = "marketing"
    print(f"--- Verifying Search Sources with query: '{query}' ---\n")
    
    sources = [
        ("Scribd", search_scribd),
        ("SlideShare", search_slideshare),
        ("Internet Archive", search_internet_archive),
        ("Google Books", search_google_books),
        ("Anna's Archive", search_annas_archive)
    ]
    
    results_summary = {}
    
    for name, func in sources:
        try:
            print(f"Testing {name}...")
            # Some functions might take different args, but current wrapper seems consistent (query)
            # Exception: search_slideshare sometimes used None for trending? Wrapper in api.py calls it with query. 
            # search_scraper logic: search_slideshare(query)
            
            results = func(query)
            count = len(results)
            print(f"  -> Success: Found {count} results")
            results_summary[name] = {"status": "OK", "count": count}
            
            if count > 0:
                print(f"  -> Sample: {results[0].get('title', 'No Title')}")
                
        except Exception as e:
            print(f"  -> FAILED: {e}")
            results_summary[name] = {"status": "ERROR", "error": str(e)}
            
    print("\n--- Summary ---")
    print(json.dumps(results_summary, indent=2))

if __name__ == "__main__":
    verify_all_sources()
