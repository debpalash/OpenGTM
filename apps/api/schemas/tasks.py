from pydantic import BaseModel
from typing import Optional


class LinkCreate(BaseModel):
    url: str


class LinkResponse(BaseModel):
    id: int
    url: str
    status: str
    created_at: str

    class Config:
        from_attributes = True


class EmailDataResponse(BaseModel):
    id: int
    name: str
    email: str
    source_link_id: int

    class Config:
        from_attributes = True
