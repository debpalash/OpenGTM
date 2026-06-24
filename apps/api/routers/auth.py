from fastapi import APIRouter, Depends, HTTPException, status
from fastapi.security import OAuth2PasswordRequestForm
from sqlalchemy.orm import Session
from datetime import datetime, timezone
from jose import JWTError
from apps.api.database import get_db
from apps.api.models import User
from apps.api.schemas.auth import Token, RefreshRequest
from apps.api.schemas.users import UserResponse
from apps.api.auth import (
    verify_password,
    create_access_token,
    create_refresh_token,
    decode_token,
    get_password_hash,
)
from apps.api.core.config import settings
from apps.api.core.security import get_current_active_user

router = APIRouter(prefix="/auth", tags=["Auth"])


@router.post("/token", response_model=Token)
async def login_for_access_token(
    form_data: OAuth2PasswordRequestForm = Depends(),
    db: Session = Depends(get_db),
):
    user = db.query(User).filter(User.username == form_data.username).first()
    if not user or not verify_password(form_data.password, user.hashed_password):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Incorrect username or password",
            headers={"WWW-Authenticate": "Bearer"},
        )
    access_token = create_access_token(data={"sub": user.username})
    refresh_token = create_refresh_token(data={"sub": user.username})
    # Update last_login
    user.last_login = datetime.now(timezone.utc)
    db.commit()
    return {
        "access_token": access_token,
        "token_type": "bearer",
        "refresh_token": refresh_token,
    }


@router.post("/refresh", response_model=Token)
async def refresh_access_token(body: RefreshRequest, db: Session = Depends(get_db)):
    """Exchange a valid refresh token for a fresh access token.

    The supplied token must carry ``type == "refresh"`` — access tokens cannot
    be replayed here. A new refresh token is also issued (sliding session).
    """
    invalid = HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Invalid refresh token",
        headers={"WWW-Authenticate": "Bearer"},
    )
    try:
        payload = decode_token(body.refresh_token)
    except JWTError:
        raise invalid
    if payload.get("type") != "refresh":
        raise invalid
    username = payload.get("sub")
    if not username:
        raise invalid
    user = db.query(User).filter(User.username == username).first()
    if user is None or not user.is_active:
        raise invalid
    return {
        "access_token": create_access_token(data={"sub": username}),
        "token_type": "bearer",
        "refresh_token": create_refresh_token(data={"sub": username}),
    }


@router.get("/me", response_model=UserResponse)
async def read_users_me(current_user: User = Depends(get_current_active_user)):
    return current_user
