from pydantic import BaseModel
from typing import Optional
from datetime import datetime


class UserBase(BaseModel):
    username: str


class UserCreate(UserBase):
    password: str


class UserUpdatePassword(BaseModel):
    password: str


class UserRoleUpdate(BaseModel):
    role: str


class UserResponse(UserBase):
    id: int
    is_active: bool
    is_admin: bool
    role: str = "user"
    profile_image: Optional[str] = None
    last_login: Optional[datetime] = None
    created_at: Optional[datetime] = None

    class Config:
        from_attributes = True
