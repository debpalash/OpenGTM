import requests
import re
import io
from bs4 import BeautifulSoup
import pypdf

# Regex for finding emails
EMAIL_REGEX = r'[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}'

class URLProcessor:
    def __init__(self):
        self.headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
        }

    def process_url(self, url):
        """
        Fetches the URL and extracts names/emails.
        Returns a dictionary with metadata and extracted data.
        """
        try:
            print(f"Fetching {url}...")
            response = requests.get(url, headers=self.headers, timeout=15, verify=False)
            response.raise_for_status()
            
            content_type = response.headers.get('Content-Type', '').lower()
            
            text_content = ""
            file_type = "unknown"
            
            if 'application/pdf' in content_type or url.lower().endswith('.pdf'):
                file_type = "pdf"
                text_content = self.extract_pdf_text(response.content)
            else:
                file_type = "html"
                text_content = self.extract_html_text(response.text)
                
            entries = self.extract_emails_from_text(text_content)
            
            return {
                "url": url,
                "file_type": file_type,
                "status": "success",
                "entries": entries,
                "count": len(entries)
            }
            
        except Exception as e:
            return {
                "url": url,
                "status": "error",
                "error": str(e),
                "entries": [],
                "count": 0
            }

    def extract_pdf_text(self, content):
        text = ""
        try:
            with io.BytesIO(content) as f:
                reader = pypdf.PdfReader(f)
                for page in reader.pages:
                    text += page.extract_text() + "\n"
        except Exception as e:
            print(f"PDF Extraction Error: {e}")
        return text

    def extract_html_text(self, html_content):
        soup = BeautifulSoup(html_content, 'html.parser')
        # Remove scripts and styles
        for script in soup(["script", "style"]):
            script.decompose()
        return soup.get_text()

    def extract_emails_from_text(self, text):
        entries = []
        seen_emails = set()
        
        lines = text.split('\n')
        
        for line in lines:
            line = line.strip()
            if not line: continue
            
            # Find all emails in line
            emails = re.findall(EMAIL_REGEX, line)
            
            for email in emails:
                if email in seen_emails: continue
                seen_emails.add(email)
                
                # Heuristic for name: Look at words before the email on the same line
                # e.g. "John Doe john.doe@example.com"
                
                # Remove the email from the line to see what remains
                remaining = line.replace(email, '').strip()
                
                # Simple cleanup of remaining text
                # We assume potential name is immediately preceding, max 3-4 words
                
                name = "Unknown"
                if remaining:
                    # Split regex to keep words
                    words = remaining.split()
                    if len(words) > 0:
                        # Take up to 3 words from the end of the previous part?
                        # Or just take the whole thing if it's short
                        clean_words = [w for w in words if len(w) > 1 and w.isalpha()] # Filter symbols
                        if clean_words:
                            candidate_name = " ".join(clean_words[-3:]) # Last 3 words
                            if len(candidate_name) > 3:
                                name = candidate_name.title()
                
                entries.append({
                    "name": name,
                    "email": email
                })
                
        return entries

url_processor = URLProcessor()
