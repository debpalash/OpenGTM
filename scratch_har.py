import json
import sys
from bs4 import BeautifulSoup

def analyze_har_html(file_path):
    print(f"Loading {file_path}...")
    with open(file_path, 'r', encoding='utf-8') as f:
        har_data = json.load(f)
        
    entries = har_data.get('log', {}).get('entries', [])
    
    for entry in entries:
        request = entry.get('request', {})
        response = entry.get('response', {})
        url = request.get('url', '')
        
        content = response.get('content', {})
        mime_type = content.get('mimeType', '')
        text = content.get('text', '')
        
        if 'text/html' in mime_type and text and 'ambitionbox.com/overview/' in url:
            print(f"Found HTML page: {url}")
            
            # Find __NEXT_DATA__
            soup = BeautifulSoup(text, 'html.parser')
            script = soup.find('script', id='__NEXT_DATA__')
            
            if script:
                data = json.loads(script.string)
                print("Found __NEXT_DATA__!")
                
                # Print the top-level keys
                props = data.get('props', {}).get('pageProps', {})
                print(f"PageProps keys: {list(props.keys())}")
                
                # Check for company details
                stats = props.get('companyStatsData', {})
                header = props.get('companyHeaderData', {})
                if header:
                    print("\nCompany Header Data:")
                    print(f"Name: {header.get('companyName')}")
                    print(f"Industry: {header.get('industry')}")
                    print(f"Rating: {header.get('rating')}")
                    
                if stats:
                    print("\nCompany Stats:")
                    print(json.dumps(stats, indent=2)[:500])
                    
                fin = props.get('companyFinancialsData', {})
                if fin:
                    print("\nFinancials:")
                    print(json.dumps(fin, indent=2)[:500])
                    
                # Look inside the initial state if using Redux/Context
                initial_state = props.get('initialState', {})
                if initial_state:
                    print(f"InitialState keys: {list(initial_state.keys())}")
                    
            else:
                print("No __NEXT_DATA__ found.")

if __name__ == "__main__":
    analyze_har_html(sys.argv[1])
