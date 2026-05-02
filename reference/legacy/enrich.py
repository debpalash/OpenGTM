import pandas as pd
import re
import time
from duckduckgo_search import DDGS

df = pd.read_csv('leads.csv')

def extract_phone(text):
    # Regex for Indian and international phone numbers
    matches = re.findall(r'(\+?91[\-\s]?\d{10}|\b\d{3,4}[\-\s]?\d{6,8}\b|\b\d{10}\b)', text)
    if matches:
        return matches[0]
    return 'N/A'

print("Starting enrichment...")
with DDGS() as ddgs:
    for index, row in df.iterrows():
        needs_phone = (pd.isna(row['Phone']) or str(row['Phone']).strip() in ('N/A', '', 'nan'))
        needs_website = (pd.isna(row['Website']) or str(row['Website']).strip() in ('N/A', '', 'nan'))
        
        if needs_phone or needs_website:
            query = f"{row['Company']} {row['City']} official website contact number"
            print(f"Searching: {query}")
            try:
                results = list(ddgs.text(query, max_results=3))
                phone_found = 'N/A'
                website_found = 'N/A'
                
                for r in results:
                    text = r.get('body', '') + " " + r.get('title', '')
                    if phone_found == 'N/A':
                        phone_found = extract_phone(text)
                    if website_found == 'N/A':
                        link = r.get('href', '')
                        if link and 'http' in link:
                            website_found = link
                
                if needs_phone and phone_found != 'N/A':
                    df.at[index, 'Phone'] = phone_found
                    print(f"  Found phone: {phone_found}")
                if needs_website and website_found != 'N/A':
                    df.at[index, 'Website'] = website_found
                    print(f"  Found website: {website_found}")
            except Exception as e:
                print(f"  Error: {e}")
            
            time.sleep(1.5)

df.to_csv('leads_enriched.csv', index=False)
print("Finished enrichment, saved to leads_enriched.csv")
