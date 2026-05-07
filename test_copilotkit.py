import asyncio
import json
import sys
sys.path.append("/Users/user4/Desktop/lead-data")
from apps.api.routers.copilotkit import _stream_chat, _build_tools, _get_active_provider

async def run():
    provider = _get_active_provider()
    tools = _build_tools()
    messages = [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "Search for leads in Pune"}
    ]
    
    async for chunk in _stream_chat(messages, tools, provider):
        print(chunk)

asyncio.run(run())
