-- Extra telemetry rows applied on top of legacy/python/scripts/contract_seed.py (base seed: spend-1/2, three
-- wb-alpha-2 activity rows, one trace, run_workbook jobs 1 and 2) BEFORE recording
-- legacy/python/scripts/contract_cases/telemetry.yaml (psql as a superuser: the whole file), and loaded by the Go tests
-- (internal/api/workbooktelemetry_test.go) section by section: `-- @ws <id>` sections run inside tenancy.InWorkspace
-- for that workspace, the `-- @super` section is skipped. All timestamps fixed.

-- @ws ws-alpha
UPDATE workbooks SET budget_spent_usd = 1.23456789 WHERE id = 'wb-alpha-1';

INSERT INTO workbook_spend_attempts (id, workspace_id, workbook_id, run_id, row_identity, column_id, provider,
  attempt_key, contract_hash, status, reserved_microusd, settled_microusd, cost_basis, created_at, updated_at)
VALUES
 ('spend-a3','ws-alpha','wb-alpha-2','run-alpha-1','row-3','email_wf','hunter','attempt-3',repeat('0',64),'uncertain',1,NULL,'{"unit":"call"}',1788000000,1788000000),
 ('spend-a4','ws-alpha','wb-alpha-2','run-alpha-1','row-4','email_wf','hunter','attempt-4',repeat('0',64),'released',5000000,NULL,'{"unit":"call"}',1788000000,1788000000);


INSERT INTO workbook_activity (workbook_id, kind, message, created_at, workspace_id) VALUES
 ('wb-alpha-2','refresh','Refreshed 25 rows','2026-08-29 10:04:00.5','ws-alpha'),
 ('wb-alpha-2','signal','Signal with <html> & "quotes" é','2026-08-29 10:05:00','ws-alpha'),
 ('wb-alpha-2',NULL,NULL,NULL,'ws-alpha');


INSERT INTO cell_traces (workbook_id, lead_id, column_id, goal, steps, outcome, total_cost_usd, created_at, workspace_id) VALUES
 ('wb-alpha-2',2,'summary','find the CEO email','[{"step":1,"provider":"hunter","success":true,"value":"x@y.z","cost":0.004,"reason":"domain match é"},{"step":2,"provider":"apollo","success":false,"value":null,"cost":0.0,"reason":"skipped"}]','found','{"spent":0.0123}','2026-08-29 10:00:00','ws-alpha'),
 ('wb-alpha-2',3,'email_wf',NULL,NULL,NULL,NULL,'2026-08-29 10:00:00','ws-alpha'),
 ('wb-alpha-2',4,'email_wf','zero cost','[]','budget','0','2026-08-29 10:00:00','ws-alpha');


INSERT INTO jobs (id, type, payload, status, priority, created_at, started_at, completed_at, error, retry_count, max_retries, last_heartbeat, next_run_at, workspace_id) VALUES
 (3,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","run_id":"run-x","row_ids":[1,2,3],"column_ids":["email_wf","summary"],"fill_missing":true,"force":false,"output_attempts":{"a":{"state":"dispatch_claimed"},"b":{"state":"recorded","result":{"success":true,"value":"secret"}},"c":{"state":"recorded","result":{"success":false}},"d":{"state":"recorded","result":{}},"e":"junk","f":{"state":"other"}},"execution_result":{"retry_count":0,"completed":5,"errors":1,"total":6,"rows":3,"stopped":false,"recorded_at":"2026-08-29T10:05:20+00:00","token":"hidden"}}','running',0,'2026-08-29 10:05:00.123456','2026-08-29 10:05:01','2026-08-29 10:05:02',NULL,0,3,'2026-08-29 10:05:30.5',NULL,'ws-alpha'),
 (4,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","run_id":"run-y","lead_ids":[5,6],"column_ids":["summary"],"row_columns":{"5":["summary"],"6":["summary"]},"execution_result":{"retry_count":1,"completed":2,"errors":0,"total":2}}','failed',0,'2026-08-29 10:06:00','2026-08-29 10:06:01','2026-08-29 10:06:09','boom é',2,3,'2026-08-29 10:06:05','2026-08-29 10:07:00','ws-alpha'),
 (5,'run_workbook','{"workbook_id":"wb-alpha-1","workspace_id":"ws-alpha","run_id":"other-wb","row_ids":[1],"column_ids":["a"]}','completed',0,'2026-08-29 10:07:00',NULL,NULL,NULL,0,3,NULL,NULL,'ws-alpha'),
 (7,'refresh_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha"}','completed',0,'2026-08-29 10:08:00',NULL,NULL,NULL,0,3,NULL,NULL,'ws-alpha'),
 (8,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","run_id":"run-z","row_ids":[1,1,2],"column_ids":["a","a"],"execution_result":{"retry_count":1,"completed":3,"errors":0,"total":3,"rows":3,"stopped":false,"recorded_at":"2026-08-29T10:09:00+00:00","extra":"hidden"}}','completed',0,'2026-08-29 10:09:00',NULL,'2026-08-29 10:09:30',NULL,1,3,NULL,NULL,'ws-alpha'),
 (9,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","run_id":42,"row_ids":[1,true],"column_ids":["x"],"fill_missing":"yes","force":0,"execution_result":{"completed":1}}','completed',0,'2026-08-29 10:10:00',NULL,NULL,NULL,NULL,3,NULL,NULL,'ws-alpha'),
 (10,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","row_ids":[1],"column_ids":["x","y"],"row_columns":{"1":["x"]},"execution_result":{"retry_count":0,"completed":1.0,"errors":0,"total":1e2,"rows":[1,{"b":2,"a":"é"}],"stopped":null}}','completed',0,'2026-08-29 10:11:00',NULL,NULL,NULL,0,3,NULL,NULL,'ws-alpha'),
 (11,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","row_ids":null,"lead_ids":[7],"column_ids":"x","output_attempts":[],"execution_result":"oops"}','queued',0,'2026-08-29 10:12:00',NULL,NULL,NULL,0,3,NULL,NULL,'ws-alpha'),
 (12,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-alpha","row_ids":[],"column_ids":[],"output_attempts":{}}','queued',0,'2026-08-29 10:13:00',NULL,NULL,NULL,0,3,NULL,NULL,'ws-alpha');


-- @ws ws-beta
INSERT INTO workbook_spend_attempts (id, workspace_id, workbook_id, run_id, row_identity, column_id, provider,
  attempt_key, contract_hash, status, reserved_microusd, settled_microusd, cost_basis, created_at, updated_at)
VALUES
 ('spend-b1','ws-beta','wb-beta-1','run-b','row-1','c','hunter','attempt-b1',repeat('0',64),'dispatched',1000000,NULL,'{"unit":"call"}',1788000000,1788000000),
 ('spend-b2','ws-beta','wb-beta-1','run-b','row-2','c','hunter','attempt-b2',repeat('0',64),'uncertain',2500000,NULL,'{"unit":"call"}',1788000000,1788000000),
 ('spend-b3','ws-beta','wb-beta-1','run-b','row-3','c','hunter','attempt-b3',repeat('0',64),'settled',900000,800000,'{"unit":"call"}',1788000000,1788000000);


INSERT INTO cell_traces (workbook_id, lead_id, column_id, goal, steps, outcome, total_cost_usd, created_at, workspace_id) VALUES
 ('wb-beta-1',1,'summary','beta trace','[]','found','{"spent":1}','2026-08-29 10:00:00','ws-beta');


INSERT INTO jobs (id, type, payload, status, priority, created_at, started_at, completed_at, error, retry_count, max_retries, last_heartbeat, next_run_at, workspace_id) VALUES
 (6,'run_workbook','{"workbook_id":"wb-alpha-2","workspace_id":"ws-beta","run_id":"other-tenant","row_ids":[1],"column_ids":["a"]}','completed',0,'2026-08-29 10:07:30',NULL,NULL,NULL,0,3,NULL,NULL,'ws-beta');


-- @super
SELECT setval(pg_get_serial_sequence('jobs','id'), 12);
