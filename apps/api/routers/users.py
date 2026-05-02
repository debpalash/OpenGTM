from fastapi import APIRouter, Depends, HTTPException
from sqlalchemy.orm import Session
from typing import List
from apps.api.database import get_db
from apps.api.models import User
from apps.api.schemas.users import (
    UserCreate,
    UserResponse,
    UserUpdatePassword,
    UserRoleUpdate,
)
from apps.api.auth import get_password_hash
from apps.api.core.security import get_current_admin_user

router = APIRouter(prefix="/admin/users", tags=["Users"])


@router.get("", response_model=List[UserResponse])
def get_users(
    db: Session = Depends(get_db), current_user: User = Depends(get_current_admin_user)
):
    return db.query(User).all()


@router.post("", response_model=UserResponse)
def create_user(
    user: UserCreate,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    db_user = db.query(User).filter(User.username == user.username).first()
    if db_user:
        raise HTTPException(status_code=400, detail="Username already registered")
    hashed_password = get_password_hash(user.password)
    db_user = User(username=user.username, hashed_password=hashed_password)
    db.add(db_user)
    db.commit()
    db.refresh(db_user)
    return db_user


@router.put("/{user_id}/password")
def reset_password(
    user_id: int,
    password_data: UserUpdatePassword,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    db_user = db.query(User).filter(User.id == user_id).first()
    if not db_user:
        raise HTTPException(status_code=404, detail="User not found")
    db_user.hashed_password = get_password_hash(password_data.password)
    db.commit()
    return {"message": "Password updated successfully"}


@router.delete("/{user_id}")
def delete_user(
    user_id: int,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    if user_id == current_user.id:
        raise HTTPException(status_code=400, detail="Cannot delete your own account")

    db_user = db.query(User).filter(User.id == user_id).first()
    if not db_user:
        raise HTTPException(status_code=404, detail="User not found")

    db.delete(db_user)
    db.commit()
    return {"message": "User deleted"}


@router.patch("/{user_id}/role", response_model=UserResponse)
def update_user_role(
    user_id: int,
    role_data: UserRoleUpdate,
    db: Session = Depends(get_db),
    current_user: User = Depends(get_current_admin_user),
):
    user = db.query(User).filter(User.id == user_id).first()
    if not user:
        raise HTTPException(status_code=404, detail="User not found")

    if role_data.role not in ["superadmin", "admin", "editor", "user", "viewer"]:
        raise HTTPException(status_code=400, detail="Invalid role")

    user.role = role_data.role
    user.is_admin = role_data.role == "admin"

    db.commit()
    db.refresh(user)
    return user
