from fastapi import Depends, HTTPException, status
from fastapi.security import OAuth2PasswordBearer
from jose import JWTError, jwt
from sqlalchemy.orm import Session
from apps.api.database import get_db
from apps.api.models import User
from apps.api.schemas.auth import TokenData
from apps.api.core.config import settings

oauth2_scheme = OAuth2PasswordBearer(tokenUrl="auth/token")


async def get_current_user(
    token: str = Depends(oauth2_scheme), db: Session = Depends(get_db)
):
    credentials_exception = HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )
    try:
        payload = jwt.decode(
            token, settings.SECRET_KEY, algorithms=[settings.ALGORITHM]
        )
        username: str = payload.get("sub")
        if username is None:
            raise credentials_exception
        token_data = TokenData(username=username)
    except JWTError:
        raise credentials_exception
    user = db.query(User).filter(User.username == token_data.username).first()
    if user is None:
        raise credentials_exception
    return user


async def get_current_active_user(current_user: User = Depends(get_current_user)):
    if not current_user.is_active:
        raise HTTPException(status_code=400, detail="Inactive user")
    return current_user


async def get_current_admin_user(current_user: User = Depends(get_current_active_user)):
    # Legacy support: check both is_admin boolean and role
    if (
        not current_user.is_admin
        and current_user.role != "admin"
        and current_user.role != "superadmin"
    ):
        raise HTTPException(status_code=403, detail="Not authorized")
    return current_user
