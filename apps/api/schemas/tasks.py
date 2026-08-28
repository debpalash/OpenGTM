from pydantic import BaseModel, ConfigDict
from typing import Optional


class LinkCreate(BaseModel):
    url: str


class LinkResponse(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: int
    url: str
    status: str
    created_at: str


class EmailDataResponse(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: int
    name: str
    email: str
    source_link_id: int
