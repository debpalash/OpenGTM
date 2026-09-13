from fastapi import Depends, HTTPException, status
from fastapi.security import OAuth2PasswordBearer
from jose import JWTError, jwt
from sqlalchemy.orm import Session
from apps.api.database import get_db
from apps.api.models import User
from apps.api.schemas.auth import TokenData
from apps.api.core.config import settings

oauth2_scheme = OAuth2PasswordBearer(tokenUrl="auth/token")


async def get_access_token_claims(token: str = Depends(oauth2_scheme)) -> dict:
    """Decode an access token once and expose its verified claims to dependencies."""
    credentials_exception = HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )
    try:
        payload = jwt.decode(token, settings.SECRET_KEY, algorithms=[settings.ALGORITHM])
        if payload.get("type") == "refresh" or not payload.get("sub"):
            raise credentials_exception
        return payload
    except JWTError as exc:
        raise credentials_exception from exc


async def get_current_user(
    claims: dict = Depends(get_access_token_claims), db: Session = Depends(get_db)
):
    credentials_exception = HTTPException(
        status_code=status.HTTP_401_UNAUTHORIZED,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )
    username: str = claims.get("sub")
    token_data = TokenData(username=username)
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


def authenticate_query_token(token: str, *, require_admin: bool = False) -> User:
    """Authenticate WebSocket/EventSource query tokens fail-closed.

    Browser WebSocket/EventSource APIs cannot attach our Authorization header,
    so these transports carry the access token in ``?token=``. Refresh tokens
    are explicitly rejected, matching :func:`get_current_user`.
    """
    from apps.api.database import SessionLocal

    if not token:
        raise HTTPException(status_code=401, detail="Missing token")
    try:
        payload = jwt.decode(token, settings.SECRET_KEY, algorithms=[settings.ALGORITHM])
        if payload.get("type") == "refresh":
            raise HTTPException(status_code=401, detail="Invalid token")
        username = payload.get("sub")
        if not username:
            raise HTTPException(status_code=401, detail="Invalid token")
    except JWTError as exc:
        raise HTTPException(status_code=401, detail="Invalid token") from exc

    with SessionLocal() as db:
        user = db.query(User).filter(User.username == username).first()
        if not user or not user.is_active:
            raise HTTPException(status_code=401, detail="Invalid token")
        if require_admin and not (
            user.is_admin or user.role in ("admin", "superadmin")
        ):
            raise HTTPException(status_code=403, detail="Not authorized")
        user._token_auth_methods = tuple(payload.get("amr") or ())
        # Detach before Session closes so callers can safely access scalar fields.
        db.expunge(user)
        return user


def enforce_workspace_sso(user: User, workspace_id: str) -> None:
    """Apply workspace SSO policy to query-token transports such as SSE/WS."""
    from apps.api.services.workspace import manager, oidc

    role = manager.member_role(workspace_id, user.id) or ""
    methods = getattr(user, "_token_auth_methods", ())
    if oidc.get_config(workspace_id).get("enforce_sso") and role != "owner" and "sso" not in methods:
        raise HTTPException(
            status_code=403,
            detail="This workspace requires SSO. Sign in with your organization identity.",
        )
