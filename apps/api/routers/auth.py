from fastapi import APIRouter, Depends, HTTPException, Request, status
from fastapi.security import OAuth2PasswordRequestForm
from fastapi.responses import RedirectResponse
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
from apps.api.services.workspace import manager as workspace_manager
from apps.api.services.workspace import oidc
from urllib.parse import quote
import logging

logger = logging.getLogger("auth.oidc")

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
    access_token = create_access_token(data={"sub": user.username, "amr": ["pwd"]})
    refresh_token = create_refresh_token(data={"sub": user.username, "amr": ["pwd"]})
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
    auth_methods = payload.get("amr") or ["pwd"]
    return {
        "access_token": create_access_token(data={"sub": username, "amr": auth_methods}),
        "token_type": "bearer",
        "refresh_token": create_refresh_token(data={"sub": username, "amr": auth_methods}),
    }


@router.get("/me", response_model=UserResponse)
async def read_users_me(current_user: User = Depends(get_current_active_user)):
    return current_user


@router.get("/sso/{workspace_slug}")
def sso_status(workspace_slug: str):
    workspace = workspace_manager.get_workspace_by_slug(workspace_slug)
    if workspace is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    config = oidc.get_config(workspace.id)
    return {"enabled": bool(config.get("enabled")), "workspace": workspace.name}


@router.get("/sso/{workspace_slug}/login")
async def sso_login(workspace_slug: str, request: Request):
    workspace = workspace_manager.get_workspace_by_slug(workspace_slug)
    if workspace is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    try:
        client = oidc.client_for(workspace.id)
    except ValueError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    redirect_uri = str(request.url_for("sso_callback", workspace_slug=workspace.slug))
    return await client.authorize_redirect(request, redirect_uri, nonce=oidc.new_nonce())


@router.get("/sso/{workspace_slug}/callback", name="sso_callback")
async def sso_callback(workspace_slug: str, request: Request, db: Session = Depends(get_db)):
    workspace = workspace_manager.get_workspace_by_slug(workspace_slug)
    if workspace is None:
        raise HTTPException(status_code=404, detail="Workspace not found")
    config = oidc.get_config(workspace.id)
    try:
        token = await oidc.client_for(workspace.id).authorize_access_token(request)
        claims = token.get("userinfo") or {}
        subject = str(claims.get("sub", ""))
        email = str(claims.get("email", "")).strip().lower()
        if not subject or not email or claims.get("email_verified") is not True:
            raise ValueError("The identity provider must return a verified email")
        domain = email.rsplit("@", 1)[-1] if "@" in email else ""
        allowed = set(config.get("allowed_domains") or [])
        if allowed and domain not in allowed:
            raise ValueError("Email domain is not allowed for this workspace")

        user_id = workspace_manager.oidc_identity_user(workspace.id, config["issuer"], subject)
        user = db.query(User).filter(User.id == user_id).first() if user_id else None
        if user is None:
            user = db.query(User).filter(User.username == email).first()
            if user is None:
                if not config.get("auto_provision"):
                    raise ValueError("No provisioned OpenGTM account matches this identity")
                user = User(username=email, hashed_password=get_password_hash(oidc.new_nonce()), role="user")
                db.add(user); db.commit(); db.refresh(user)
            if not workspace_manager.is_member(workspace.id, user.id):
                if not config.get("auto_provision"):
                    raise ValueError("This account is not provisioned in the workspace")
                workspace_manager.add_member(workspace.id, user.id, config.get("default_role", "viewer"))
            workspace_manager.bind_oidc_identity(workspace.id, config["issuer"], subject, user.id, email)
        if not user.is_active or not workspace_manager.is_member(workspace.id, user.id):
            raise ValueError("This SSO identity is not an active workspace member")
        user.last_login = datetime.now(timezone.utc); db.commit()
        workspace_manager.set_user_active_workspace(user.id, workspace.id)
        access = create_access_token(data={"sub": user.username, "amr": ["sso"]})
        return RedirectResponse(f"/login#sso_access_token={quote(access)}")
    except ValueError as exc:
        return RedirectResponse(f"/login#sso_error={quote(str(exc)[:200])}", status_code=303)
    except Exception as exc:
        logger.warning("OIDC callback failed workspace=%s: %s", workspace.id, type(exc).__name__)
        return RedirectResponse("/login#sso_error=SSO%20authentication%20failed", status_code=303)
