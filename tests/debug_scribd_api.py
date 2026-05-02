
import requests
import json
import logging

# Headers provided by the user (sanitized/simplified where possible, but keeping cookies)
HEADERS = {
    'User-Agent': 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:146.0) Gecko/20100101 Firefox/146.0',
    'Accept': 'application/json',
    'Accept-Language': 'en-US,en;q=0.5',
    'Referer': 'https://www.scribd.com/search?query=indian%20university%20email&verbatim=true',
    'X-Requested-With': 'XMLHttpRequest',
    'Content-Type': 'application/json',
    'Connection': 'keep-alive',
    'Cookie': 'scribd_ubtc=u%3D9ac34f16-e8d0-4580-a5df-794be53a915d%26h%3DYHkzszW%2F%2FghEhqOO0X1Z4gvH5kGBcxHnSSY9CQ4EZq4%3D; _scribd_session=ak1NbEhkL281UVJrY2JRdlBUbHhoRU14WlJCaXAzYmVPbDJjWmJQWGZOLzJlT2VSdDVUdUJCSklySitNbGkrRWdlZGNJOU9LbkkrK2RpM2w4QnNRS0YvYTJpMnpQclFBb2N2MHVsUnU1b3AwUHREcmFvYlh3TUxRUnBDQ3U2MGh2aDZybjVhbzlHWkEwekZBYkw3TEhBdEtxVDVtZlJoZTJIUzdZVGRIMzkwS2tjSDJUNTBRMDZTZU9pU2RmWVNhc1pZMWRYVWNFVis3cFhaenJzT0laeW5QVzM2dzJqUlVGSlVTaExkdEl4SGlLNXJJN1MwdjI1Sk13NXNwQ3ZHR053cEFPUy91ejRiUjhERGpOODEwYjY1V3VQM29DNFZEZ1c5S05mSlVhcjZrN1BHdXc2cHVsSWExV0N4TjBOYnctLW5haXYzOXVwL09KYVNHM01BNnhRM1E9PQ%3D%3D--68678ffd96a50993cd6a7941fc188c471ff1c0f4;'
}

def test_direct_search():
    url = 'https://www.scribd.com/search/query?query=python&verbatim=true'
    print(f"Testing direct search: {url}")
    try:
        resp = requests.get(url, headers=HEADERS, timeout=10)
        print(f"Status: {resp.status_code}")
        if resp.status_code == 200:
            data = resp.json()
            print("Keys in root:", data.keys())
            if 'results' in data:
                results = data['results']
                print("Keys in results:", results.keys())
                if 'documents' in results:
                    docs = results['documents']
                    print("Keys in documents:", docs.keys())
                    if 'content' in docs:
                         content = docs['content']
                         # Is content a dict or list?
                         print(f"Content type: {type(content)}")
                         if isinstance(content, dict):
                            print("Keys in content:", content.keys())
                            # It seems 'items' isn't here, maybe 'documents'?
                            if 'documents' in content:
                                doc_data = content['documents']
                                print(f"Documents type: {type(doc_data)}")
                                if isinstance(doc_data, dict) and 'items' in doc_data:
                                    items = doc_data['items']
                                    print(f"Found {len(items)} items in documents.items")
                                    if len(items) > 0:
                                        print("First item sample:", json.dumps(items[0], indent=2))
                                elif isinstance(doc_data, list):
                                     print(f"Found {len(doc_data)} items in documents list")
                                     if len(doc_data) > 0:
                                        print("First item sample:", json.dumps(doc_data[0], indent=2))
        else:
            print("Response:", resp.text[:500])
            
    except Exception as e:
        print(f"Error: {e}")

if __name__ == "__main__":
    test_direct_search()
