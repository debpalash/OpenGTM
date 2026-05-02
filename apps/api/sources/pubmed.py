"""
PubMed Central source adapter - NCBI's free full-text archive
"""
import aiohttp
from typing import List, Optional
import urllib.parse
import logging
import xml.etree.ElementTree as ET

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class PubMedSource(DocumentSource):
    """PubMed Central search adapter (NCBI E-utilities API)"""
    
    SEARCH_URL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi"
    SUMMARY_URL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esummary.fcgi"
    
    @property
    def name(self) -> str:
        return "pubmed"
    
    @property
    def display_name(self) -> str:
        return "PubMed Central"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search PubMed Central using E-utilities API"""
        results = []
        
        try:
            # Step 1: Search for PMC IDs
            search_params = {
                "db": "pmc",
                "term": query,
                "retmax": limit,
                "retmode": "xml"
            }
            
            async with aiohttp.ClientSession() as session:
                # Get PMC IDs
                async with session.get(self.SEARCH_URL, params=search_params, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"PubMed search API returned {response.status}")
                        return results
                    
                    xml_text = await response.text()
                    root = ET.fromstring(xml_text)
                    
                    id_list = root.find('IdList')
                    if id_list is None:
                        return results
                    
                    pmc_ids = [id_elem.text for id_elem in id_list.findall('Id')]
                    
                    if not pmc_ids:
                        return results
                    
                    # Step 2: Get summaries for these IDs
                    summary_params = {
                        "db": "pmc",
                        "id": ','.join(pmc_ids),
                        "retmode": "xml"
                    }
                    
                    async with session.get(self.SUMMARY_URL, params=summary_params, timeout=15) as response:
                        if response.status != 200:
                            return results
                        
                        xml_text = await response.text()
                        root = ET.fromstring(xml_text)
                        
                        for doc_sum in root.findall('.//DocSum'):
                            try:
                                pmc_id = doc_sum.find('Id').text
                                
                                # Extract fields
                                title = None
                                authors = []
                                pub_date = None
                                
                                for item in doc_sum.findall('Item'):
                                    name = item.get('Name')
                                    if name == 'Title':
                                        title = item.text
                                    elif name == 'AuthorList':
                                        authors = [a.text for a in item.findall('.//Item')[:3]]
                                    elif name == 'PubDate':
                                        pub_date = item.text
                                
                                if not title:
                                    continue
                                
                                # Build author string
                                author_str = ', '.join(authors) if authors else None
                                
                                # Extract year
                                year = None
                                if pub_date:
                                    try:
                                        year = int(pub_date.split()[0])
                                    except:
                                        pass
                                
                                # URLs
                                url = f"https://www.ncbi.nlm.nih.gov/pmc/articles/PMC{pmc_id}/"
                                download_url = f"https://www.ncbi.nlm.nih.gov/pmc/articles/PMC{pmc_id}/pdf/"
                                
                                result = SearchResult(
                                    id=f"pubmed_{pmc_id}",
                                    title=title,
                                    url=url,
                                    source=self.name,
                                    download_url=download_url,
                                    author=author_str,
                                    year=year,
                                    file_type=FileType.PDF,
                                    snippet="PubMed Central free full-text"
                                )
                                results.append(result)
                                
                            except Exception as e:
                                logger.error(f"Error parsing PubMed result: {e}")
                                continue
                    
        except Exception as e:
            logger.error(f"PubMed search error: {e}")
        
        return results
