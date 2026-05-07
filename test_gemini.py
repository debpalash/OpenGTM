import httpx
import asyncio

async def test():
    api_key = "***REMOVED***"
    url1 = "https://generativelanguage.googleapis.com/v1beta/chat/completions"
    headers1 = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }
    
    body = {
        "model": "gemini-2.5-flash",
        "messages": [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": ""}
        ],
    }
    
    async with httpx.AsyncClient() as client:
        r1 = await client.post(url1, json=body, headers=headers1)
        print("Empty assistant status:", r1.status_code)
        print("Empty assistant text:", r1.text)

asyncio.run(test())
