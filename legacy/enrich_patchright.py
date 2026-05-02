import asyncio
import pandas as pd
import re
from bs4 import BeautifulSoup
from patchright.async_api import async_playwright
from duckduckgo_search import DDGS
import sys

def extract_phone(text):
    # Regex for Indian and international phone numbers
    # Basic matching for 10 digits, +91, 1800-etc
    matches = re.findall(r'(\+?91[\-\s]?\d{10}|\b\d{3,4}[\-\s]?\d{6,8}\b|\b\d{10}\b)', text)
    if matches:
        return matches[0]
    return 'N/A'

async def fetch_phone_from_url(context, url):
    if not url or url == 'N/A' or pd.isna(url):
        return 'N/A'
    if not url.startswith('http'):
        url = 'https://' + url
    
    page = await context.new_page()
    phone = 'N/A'
    try:
        # Load the page, timeout 15s
        await page.goto(url, wait_until='domcontentloaded', timeout=15000)
        content = await page.content()
        soup = BeautifulSoup(content, 'html.parser')
        text = soup.get_text(separator=' ')
        
        # Try finding a phone number directly in text
        found = extract_phone(text)
        if found != 'N/A':
            phone = found
        else:
            # Maybe check links with tel:
            for a in soup.find_all('a', href=True):
                if a['href'].startswith('tel:'):
                    phone = a['href'].replace('tel:', '').strip()
                    break
    except Exception as e:
        pass
    finally:
        await page.close()
    return phone

async def process_leads():
    try:
        df1 = pd.read_csv('leads.csv')
    except:
        df1 = pd.DataFrame()
        
    try:
        df2 = pd.read_csv('leads2.csv')
    except:
        df2 = pd.DataFrame()

    # Normalize columns
    # df1: Company,Website,Email,Phone,City,Notes
    # df2: Company,Website,City,Specialization
    
    if not df1.empty and not df2.empty:
        df = pd.concat([df1, df2], ignore_index=True)
    elif not df1.empty:
        df = df1
    else:
        df = df2

    if 'Phone' not in df.columns:
        df['Phone'] = 'N/A'
    if 'Email' not in df.columns:
        df['Email'] = 'N/A'
    if 'Notes' not in df.columns:
        df['Notes'] = 'N/A'
    if 'Specialization' not in df.columns:
        df['Specialization'] = 'HR Services'

    df['Yupcha Value Prop'] = 'Unified Superadmin Console for multi-database CRM tracking, candidate visibility, and automated reporting'
    df['Company Need'] = 'Centralized Candidate, Attendance, and Client Management System'

    # Deduplicate by Company
    df = df.drop_duplicates(subset=['Company'], keep='first').reset_index(drop=True)

    print(f"Total leads to process: {len(df)}")
    
    # Check DuckDuckGo for missing websites first
    print("Finding missing websites...")
    with DDGS() as ddgs:
        for idx, row in df.iterrows():
            if pd.isna(row['Website']) or str(row['Website']).strip() in ('N/A', '', 'nan'):
                query = f"{row['Company']} {row['City']} official website"
                try:
                    res = list(ddgs.text(query, max_results=1))
                    if res and 'href' in res[0]:
                        df.at[idx, 'Website'] = res[0]['href']
                except:
                    pass

    # Use patchright for finding missing phone numbers from websites
    print("Extracting site data with stealth headless mode...")
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        # using generic context, could use stealth plugins if needed, patchright handles basic evasion
        context = await browser.new_context()
        
        # We will process in batches to speed it up
        batch_size = 5
        for i in range(0, len(df), batch_size):
            batch = df.iloc[i:i+batch_size]
            tasks = []
            for idx, row in batch.iterrows():
                if pd.isna(row['Phone']) or str(row['Phone']).strip() in ('N/A', '', 'nan', '[email protected]'):
                    tasks.append((idx, fetch_phone_from_url(context, row['Website'])))
            
            if tasks:
                results = await asyncio.gather(*(t[1] for t in tasks))
                for (idx, _), phone in zip(tasks, results):
                    if phone != 'N/A':
                        df.at[idx, 'Phone'] = phone
                        print(f"[{df.at[idx, 'Company']}] Found phone: {phone}")
                    else:
                        print(f"[{df.at[idx, 'Company']}] Phone not found on site")
                        
        await browser.close()

    df.to_csv('leads_final.csv', index=False)
    print("Data saved to leads_final.csv")

if __name__ == "__main__":
    asyncio.run(process_leads())
