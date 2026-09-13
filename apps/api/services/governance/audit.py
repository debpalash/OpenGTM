import logging
import re
import uuid

from starlette.middleware.base import BaseHTTPMiddleware

logger = logging.getLogger("governance.audit")
WRITE_METHODS = {"POST", "PUT", "PATCH", "DELETE"}
SAFE_REQUEST_ID = re.compile(r"^[A-Za-z0-9._:-]{1,64}$")


class GovernanceAuditMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request, call_next):
        supplied = request.headers.get("x-request-id", "")
        request_id = supplied if SAFE_REQUEST_ID.fullmatch(supplied) else str(uuid.uuid4())
        request.state.request_id = request_id
        try:
            response = await call_next(request)
            status_code = response.status_code
        except Exception:
            status_code = 500
            self._record(request, request_id, status_code)
            raise
        response.headers["X-Request-Id"] = request_id
        self._record(request, request_id, status_code)
        return response

    @staticmethod
    def _record(request, request_id, status_code):
        workspace_id = getattr(request.state, "workspace_id", None)
        if request.method in WRITE_METHODS and workspace_id:
            route_obj = request.scope.get("route")
            route = getattr(route_obj, "path", request.url.path)
            try:
                from apps.api.core.tenancy import workspace_scope
                from apps.api.database import SessionLocal
                from apps.api.services.governance.models import GovernanceAuditEvent
                with workspace_scope(workspace_id):
                    with SessionLocal() as db, db.begin():
                        db.add(GovernanceAuditEvent(
                            workspace_id=workspace_id,
                            actor_user_id=getattr(request.state, "actor_user_id", None),
                            actor_role=getattr(request.state, "actor_role", ""),
                            method=request.method,
                            route=route[:255],
                            resource_path=request.url.path[:500],
                            response_status=status_code,
                            outcome="success" if status_code < 400 else "denied" if status_code in {401, 403} else "error",
                            request_id=request_id,
                            metadata_json={},
                        ))
            except Exception as exc:
                logger.error("governance audit write failed request_id=%s: %s", request_id, exc)
