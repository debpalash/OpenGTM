--
-- PostgreSQL database dump
--

-- Dumped from database version 18.6 (Debian 18.6-1.pgdg13+2)
-- Dumped by pg_dump version 18.6 (Debian 18.6-1.pgdg13+2)

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;

--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';

--
-- Name: leads_search_tsv_trigger(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.leads_search_tsv_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
        BEGIN
            NEW.search_tsv := to_tsvector('english', coalesce(NEW.company, '') || ' ' || coalesce(NEW.city, '') || ' ' || coalesce(NEW.specialization, '') || ' ' || coalesce(NEW.notes, '') || ' ' || coalesce(NEW.description, ''));
            RETURN NEW;
        END
        $$;

--
-- Name: outreach_send_by_message_id(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.outreach_send_by_message_id(p_message_id text) RETURNS TABLE(workspace_id text, send_id integer, sequence_id text, enrollment_id integer, to_email text)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public', 'pg_temp'
    AS $$
            SELECT s.workspace_id::text, s.id, s.sequence_id::text,
                   s.enrollment_id, s.to_email::text
            FROM public.outreach_sends s
            WHERE s.message_id = p_message_id
            ORDER BY s.id DESC
            LIMIT 1
        $$;

REVOKE ALL ON FUNCTION public.outreach_send_by_message_id(p_message_id text) FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'yupcha_app') THEN
        GRANT EXECUTE ON FUNCTION public.outreach_send_by_message_id(p_message_id text) TO yupcha_app;
    END IF;
END
$$;

--
-- Name: workbook_workspace_by_id(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.workbook_workspace_by_id(p_workbook_id text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public', 'pg_temp'
    AS $$
        SELECT w.workspace_id::text FROM public.workbooks w WHERE w.id = p_workbook_id
    $$;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: alembic_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.alembic_version (
    version_num character varying(32) NOT NULL
);

--
-- Name: app_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_settings (
    key text NOT NULL,
    value text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: audience_destinations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audience_destinations (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    audience_id character varying NOT NULL,
    name character varying(200) NOT NULL,
    destination_type character varying(32) NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    config json NOT NULL,
    field_map json NOT NULL,
    health_status character varying(20) DEFAULT 'unverified'::character varying NOT NULL,
    last_error text,
    last_success_at timestamp without time zone,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.audience_destinations FORCE ROW LEVEL SECURITY;

--
-- Name: audience_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audience_members (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    audience_id character varying NOT NULL,
    lead_id integer NOT NULL,
    snapshot json NOT NULL,
    joined_at timestamp without time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp without time zone DEFAULT now() NOT NULL,
    refresh_token character varying(36)
);

ALTER TABLE ONLY public.audience_members FORCE ROW LEVEL SECURITY;

--
-- Name: audience_members_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.audience_members_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: audience_members_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.audience_members_id_seq OWNED BY public.audience_members.id;

--
-- Name: audience_membership_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audience_membership_events (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    audience_id character varying NOT NULL,
    lead_id integer NOT NULL,
    event_type character varying(16) NOT NULL,
    snapshot json NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    refresh_id character varying(36)
);

ALTER TABLE ONLY public.audience_membership_events FORCE ROW LEVEL SECURITY;

--
-- Name: audience_membership_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.audience_membership_events_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: audience_membership_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.audience_membership_events_id_seq OWNED BY public.audience_membership_events.id;

--
-- Name: audience_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audience_schedules (
    audience_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    next_refresh_at timestamp without time zone,
    enabled boolean DEFAULT true NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

--
-- Name: audiences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audiences (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    name character varying(200) NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    filters json NOT NULL,
    member_count integer DEFAULT 0 NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL,
    refreshed_at timestamp without time zone,
    refresh_enabled boolean DEFAULT true NOT NULL,
    refresh_interval_minutes integer DEFAULT 60 NOT NULL,
    next_refresh_at timestamp without time zone,
    refresh_health character varying(20) DEFAULT 'unverified'::character varying NOT NULL,
    last_refresh_error text,
    consecutive_refresh_failures integer DEFAULT 0 NOT NULL
);

ALTER TABLE ONLY public.audiences FORCE ROW LEVEL SECURITY;

--
-- Name: cell_traces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cell_traces (
    id integer NOT NULL,
    workbook_id character varying NOT NULL,
    lead_id integer NOT NULL,
    column_id character varying NOT NULL,
    goal character varying(512),
    steps json,
    outcome character varying(32),
    total_cost_usd json,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.cell_traces FORCE ROW LEVEL SECURITY;

--
-- Name: cell_traces_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cell_traces_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: cell_traces_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cell_traces_id_seq OWNED BY public.cell_traces.id;

--
-- Name: chat_conversations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.chat_conversations (
    id text NOT NULL,
    title text DEFAULT 'New Chat'::text NOT NULL,
    workspace_id character varying(64) NOT NULL,
    user_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.chat_conversations FORCE ROW LEVEL SECURITY;

--
-- Name: chat_memories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.chat_memories (
    id bigint NOT NULL,
    user_id text NOT NULL,
    text text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone NOT NULL
);

--
-- Name: chat_memories_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.chat_memories_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: chat_memories_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.chat_memories_id_seq OWNED BY public.chat_memories.id;

--
-- Name: chat_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.chat_messages (
    id text NOT NULL,
    conversation_id text NOT NULL,
    workspace_id character varying(64) NOT NULL,
    role text NOT NULL,
    content text DEFAULT ''::text NOT NULL,
    tool_data text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ck_chat_messages_role CHECK ((role = ANY (ARRAY['user'::text, 'assistant'::text, 'system'::text, 'tool'::text])))
);

ALTER TABLE ONLY public.chat_messages FORCE ROW LEVEL SECURITY;

--
-- Name: chat_tool_approvals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.chat_tool_approvals (
    id text NOT NULL,
    workspace_id character varying(64) NOT NULL,
    user_id text NOT NULL,
    tool_call_json text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    created_at_epoch bigint NOT NULL
);

ALTER TABLE ONLY public.chat_tool_approvals FORCE ROW LEVEL SECURITY;

--
-- Name: collection_job_stages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_job_stages (
    id bigint NOT NULL,
    workspace_id text NOT NULL,
    job_id text NOT NULL,
    stage text NOT NULL,
    status text DEFAULT 'running'::text NOT NULL,
    input_count integer DEFAULT 0 NOT NULL,
    output_count integer DEFAULT 0 NOT NULL,
    rejected_count integer DEFAULT 0 NOT NULL,
    details text DEFAULT '{}'::text NOT NULL,
    started_at text DEFAULT ''::text NOT NULL,
    completed_at text DEFAULT ''::text NOT NULL
);

ALTER TABLE ONLY public.collection_job_stages FORCE ROW LEVEL SECURITY;

--
-- Name: collection_job_stages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.collection_job_stages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: collection_job_stages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.collection_job_stages_id_seq OWNED BY public.collection_job_stages.id;

--
-- Name: collection_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.collection_jobs (
    id text NOT NULL,
    workspace_id text NOT NULL,
    query text NOT NULL,
    intent text DEFAULT 'market_search'::text NOT NULL,
    intent_details text DEFAULT '{}'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    tier integer DEFAULT 1 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    max_attempts integer DEFAULT 3 NOT NULL,
    leads_found integer DEFAULT 0 NOT NULL,
    proxy_used text DEFAULT ''::text NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    created_at text DEFAULT ''::text NOT NULL,
    started_at text DEFAULT ''::text NOT NULL,
    completed_at text DEFAULT ''::text NOT NULL
);

ALTER TABLE ONLY public.collection_jobs FORCE ROW LEVEL SECURITY;

--
-- Name: company_entities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.company_entities (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    canonical_name character varying(512) NOT NULL,
    primary_domain character varying(255),
    primary_phone character varying(64),
    primary_email character varying(255),
    primary_city character varying(128),
    identity_keys json,
    fields json,
    sources json,
    source_agreement json,
    corroboration_count integer,
    observation_count integer,
    first_seen timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    last_seen timestamp without time zone DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE ONLY public.company_entities FORCE ROW LEVEL SECURITY;

--
-- Name: company_identifiers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.company_identifiers (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    kind character varying(32) NOT NULL,
    value character varying(255) NOT NULL,
    entity_id character varying NOT NULL,
    created_at timestamp without time zone DEFAULT now()
);

ALTER TABLE ONLY public.company_identifiers FORCE ROW LEVEL SECURITY;

--
-- Name: company_identifiers_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.company_identifiers_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: company_identifiers_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.company_identifiers_id_seq OWNED BY public.company_identifiers.id;

--
-- Name: connector_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.connector_runs (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    workbook_id character varying NOT NULL,
    connector character varying(100) NOT NULL,
    status character varying(32) NOT NULL,
    query json,
    cursor json,
    requested_count integer NOT NULL,
    fetched_count integer NOT NULL,
    added_count integer NOT NULL,
    updated_count integer NOT NULL,
    skipped_count integer NOT NULL,
    pages_fetched integer NOT NULL,
    source_total integer,
    target_met boolean NOT NULL,
    exhausted boolean NOT NULL,
    error text,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.connector_runs FORCE ROW LEVEL SECURITY;

--
-- Name: contact_executions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.contact_executions (
    id character varying(36) NOT NULL,
    workspace_id character varying(255) NOT NULL,
    action_id character varying(255) NOT NULL,
    contract_hash character varying(64) NOT NULL,
    status character varying(20) NOT NULL,
    started_at double precision NOT NULL,
    expires_at double precision NOT NULL,
    result json
);

--
-- Name: credit_ledger_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.credit_ledger_entries (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    amount_usd double precision NOT NULL,
    reason character varying(64) NOT NULL,
    run_id character varying(128),
    idempotency_key character varying(160),
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE ONLY public.credit_ledger_entries FORCE ROW LEVEL SECURITY;

--
-- Name: credit_ledger_entries_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.credit_ledger_entries_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: credit_ledger_entries_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.credit_ledger_entries_id_seq OWNED BY public.credit_ledger_entries.id;

--
-- Name: destination_deliveries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.destination_deliveries (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    run_id character varying NOT NULL,
    destination_id character varying NOT NULL,
    lead_id integer NOT NULL,
    operation character varying(16) NOT NULL,
    idempotency_key character varying(255) NOT NULL,
    payload_fingerprint character varying(64) NOT NULL,
    status character varying(20) NOT NULL,
    attempts integer NOT NULL,
    external_id character varying,
    summary text,
    error text,
    delivered_at timestamp without time zone,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL,
    payload json DEFAULT '{}'::json NOT NULL
);

ALTER TABLE ONLY public.destination_deliveries FORCE ROW LEVEL SECURITY;

--
-- Name: destination_deliveries_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.destination_deliveries_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: destination_deliveries_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.destination_deliveries_id_seq OWNED BY public.destination_deliveries.id;

--
-- Name: destination_inbound_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.destination_inbound_receipts (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    destination_id character varying NOT NULL,
    provider character varying(32) NOT NULL,
    external_event_id character varying(255) NOT NULL,
    external_record_id character varying(255),
    lead_id integer,
    status character varying(24) NOT NULL,
    conflict_policy character varying(24) NOT NULL,
    applied_fields json NOT NULL,
    ignored_fields json NOT NULL,
    payload_fingerprint character varying(64) NOT NULL,
    error text,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.destination_inbound_receipts FORCE ROW LEVEL SECURITY;

--
-- Name: destination_inbound_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.destination_inbound_tokens (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    destination_id character varying NOT NULL,
    token_hash character varying(64) NOT NULL,
    prefix character varying(20) NOT NULL,
    created_by integer,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    revoked_at timestamp without time zone,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone
);

--
-- Name: destination_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.destination_runs (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    destination_id character varying NOT NULL,
    status character varying(32) DEFAULT 'pending'::character varying NOT NULL,
    requested_by character varying,
    attempted integer DEFAULT 0 NOT NULL,
    succeeded integer DEFAULT 0 NOT NULL,
    failed integer DEFAULT 0 NOT NULL,
    skipped integer DEFAULT 0 NOT NULL,
    error text,
    started_at timestamp without time zone,
    finished_at timestamp without time zone,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.destination_runs FORCE ROW LEVEL SECURITY;

--
-- Name: email_data; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.email_data (
    id integer NOT NULL,
    name character varying,
    email character varying,
    source_link_id integer,
    created_at character varying,
    is_used boolean,
    tags character varying,
    notes text
);

--
-- Name: email_data_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.email_data_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: email_data_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.email_data_id_seq OWNED BY public.email_data.id;

--
-- Name: email_verify_cache; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.email_verify_cache (
    workspace_id text NOT NULL,
    email_hash text NOT NULL,
    result jsonb NOT NULL,
    cached_at timestamp without time zone NOT NULL,
    expires_at timestamp without time zone NOT NULL
);

ALTER TABLE ONLY public.email_verify_cache FORCE ROW LEVEL SECURITY;

--
-- Name: enrichment_cache; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.enrichment_cache (
    workspace_id text NOT NULL,
    cache_key text NOT NULL,
    field_name text NOT NULL,
    value jsonb,
    provider text,
    confidence double precision,
    cached_at timestamp without time zone NOT NULL,
    expires_at timestamp without time zone NOT NULL
);

ALTER TABLE ONLY public.enrichment_cache FORCE ROW LEVEL SECURITY;

--
-- Name: entity_blocking_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entity_blocking_keys (
    id integer NOT NULL,
    key character varying(255) NOT NULL,
    entity_id character varying NOT NULL,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.entity_blocking_keys FORCE ROW LEVEL SECURITY;

--
-- Name: entity_blocking_keys_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.entity_blocking_keys_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: entity_blocking_keys_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.entity_blocking_keys_id_seq OWNED BY public.entity_blocking_keys.id;

--
-- Name: entity_merge_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entity_merge_log (
    id integer NOT NULL,
    kept_id character varying,
    merged_id character varying,
    reason character varying(255),
    score double precision,
    snapshot json,
    reverted integer,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.entity_merge_log FORCE ROW LEVEL SECURITY;

--
-- Name: entity_merge_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.entity_merge_log_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: entity_merge_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.entity_merge_log_id_seq OWNED BY public.entity_merge_log.id;

--
-- Name: entity_review_pairs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.entity_review_pairs (
    id integer NOT NULL,
    entity_id character varying,
    candidate json,
    score double precision,
    reason character varying(255),
    status character varying(32),
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.entity_review_pairs FORCE ROW LEVEL SECURITY;

--
-- Name: entity_review_pairs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.entity_review_pairs_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: entity_review_pairs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.entity_review_pairs_id_seq OWNED BY public.entity_review_pairs.id;

--
-- Name: governance_audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.governance_audit_events (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    actor_user_id integer,
    actor_role character varying(32) NOT NULL,
    method character varying(10) NOT NULL,
    route character varying(255) NOT NULL,
    resource_path character varying(500) NOT NULL,
    response_status integer NOT NULL,
    outcome character varying(20) NOT NULL,
    request_id character varying(64) NOT NULL,
    metadata_json json NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.governance_audit_events FORCE ROW LEVEL SECURITY;

--
-- Name: jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.jobs (
    id integer NOT NULL,
    type character varying,
    payload json,
    status character varying,
    priority integer,
    created_at timestamp without time zone,
    started_at timestamp without time zone,
    completed_at timestamp without time zone,
    error character varying,
    retry_count integer,
    max_retries integer,
    last_heartbeat timestamp without time zone,
    worker_id character varying,
    next_run_at timestamp without time zone,
    locked_at timestamp without time zone,
    fire_key character varying,
    workspace_id character varying,
    executor text DEFAULT 'python'::text NOT NULL,
    lease_token bigint DEFAULT '0'::bigint NOT NULL,
    CONSTRAINT ck_jobs_executor CHECK ((executor = ANY (ARRAY['python'::text, 'go'::text])))
);

--
-- Name: jobs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.jobs_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: jobs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.jobs_id_seq OWNED BY public.jobs.id;

--
-- Name: leads; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leads (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    company character varying NOT NULL,
    website character varying,
    email character varying,
    email_confidence character varying,
    email_provider character varying,
    phone character varying,
    phone_provider character varying,
    contact_person character varying,
    contact_title character varying,
    city character varying,
    state character varying,
    address character varying,
    specialization character varying,
    company_size character varying,
    employee_count_exact integer,
    description text,
    revenue_range character varying,
    founded_year character varying,
    founding_year character varying,
    industry_tags text,
    technologies text,
    funding_stage character varying,
    linkedin_url character varying,
    twitter_url character varying,
    facebook_url character varying,
    secondary_emails text,
    secondary_phones text,
    decision_makers text,
    glassdoor_rating character varying,
    hiring_signals text,
    enrichment_attempts integer,
    enrichment_waterfall text,
    last_funding_amount character varying,
    investors text,
    recent_news text,
    google_rating character varying,
    revenue_estimate character varying,
    email_verify character varying,
    email_presence character varying,
    source character varying,
    score integer,
    score_tier character varying,
    status character varying,
    yupcha_value_prop text,
    company_need text,
    notes text,
    created_at character varying,
    updated_at character varying,
    last_enriched_at character varying,
    search_tsv tsvector,
    company_size_basis character varying DEFAULT ''::character varying,
    field_provenance text,
    technographics text,
    source_url text,
    collection_job_id character varying
);

ALTER TABLE ONLY public.leads FORCE ROW LEVEL SECURITY;

--
-- Name: leads_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.leads_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: leads_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.leads_id_seq OWNED BY public.leads.id;

--
-- Name: links; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.links (
    id integer NOT NULL,
    url character varying,
    status character varying,
    created_at character varying,
    source character varying,
    updated_at character varying
);

--
-- Name: links_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.links_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: links_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.links_id_seq OWNED BY public.links.id;

--
-- Name: llm_usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.llm_usage_daily (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    provider character varying(64) NOT NULL,
    model character varying(255) DEFAULT ''::character varying NOT NULL,
    date character varying(10) NOT NULL,
    calls integer DEFAULT 0 NOT NULL,
    prompt_tokens integer DEFAULT 0 NOT NULL,
    completion_tokens integer DEFAULT 0 NOT NULL,
    total_tokens integer DEFAULT 0 NOT NULL,
    rate_limit integer DEFAULT 0 NOT NULL,
    rate_remaining integer DEFAULT 0 NOT NULL,
    rate_reset character varying(255) DEFAULT ''::character varying NOT NULL,
    updated_at character varying NOT NULL
);

ALTER TABLE ONLY public.llm_usage_daily FORCE ROW LEVEL SECURITY;

--
-- Name: llm_usage_daily_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.llm_usage_daily_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: llm_usage_daily_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.llm_usage_daily_id_seq OWNED BY public.llm_usage_daily.id;

--
-- Name: mcp_audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_audit_log (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    token_id character varying(36),
    user_id integer,
    tool_name character varying(100) NOT NULL,
    arguments_redacted json NOT NULL,
    idempotency_key character varying(128),
    result_status character varying(20) DEFAULT 'ok'::character varying NOT NULL,
    error text,
    estimated_cost_usd double precision DEFAULT '0'::double precision NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.mcp_audit_log FORCE ROW LEVEL SECURITY;

--
-- Name: mcp_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_tokens (
    id character varying(36) NOT NULL,
    token_hash character varying(64) NOT NULL,
    prefix character varying(20) NOT NULL,
    name character varying(200) DEFAULT ''::character varying NOT NULL,
    user_id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    capabilities json NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by integer
);

--
-- Name: outreach_drafts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_drafts (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    action_idempotency_key character varying(255) NOT NULL,
    conversation_id character varying(64) NOT NULL,
    source_action_id character varying(255) NOT NULL,
    person_id character varying(80) NOT NULL,
    person_name character varying(200) NOT NULL,
    company character varying(200) NOT NULL,
    title character varying(240),
    to_email character varying(320) NOT NULL,
    contact_status character varying(20) NOT NULL,
    risky_approved boolean DEFAULT false NOT NULL,
    generic_inbox boolean DEFAULT false NOT NULL,
    is_role_address boolean DEFAULT false NOT NULL,
    subject character varying(300) NOT NULL,
    body_text text NOT NULL,
    sentence_evidence json NOT NULL,
    source_snapshot json NOT NULL,
    state character varying(20) DEFAULT 'draft'::character varying NOT NULL,
    sent_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.outreach_drafts FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_enrollments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_enrollments (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    sequence_id character varying(36) NOT NULL,
    lead_id integer NOT NULL,
    to_email_snapshot character varying NOT NULL,
    consent_source character varying(120),
    consent_at timestamp with time zone,
    current_step integer,
    status character varying(20),
    next_send_at timestamp with time zone,
    sent_count integer,
    soft_bounce_count integer,
    last_sent_at timestamp with time zone,
    error text
);

ALTER TABLE ONLY public.outreach_enrollments FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_enrollments_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.outreach_enrollments_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: outreach_enrollments_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.outreach_enrollments_id_seq OWNED BY public.outreach_enrollments.id;

--
-- Name: outreach_inbound_messages; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_inbound_messages (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    imap_uid character varying(64) DEFAULT ''::character varying NOT NULL,
    uidvalidity character varying(64) DEFAULT ''::character varying NOT NULL,
    source_message_id character varying DEFAULT ''::character varying NOT NULL,
    matched_send_id integer,
    kind character varying(20),
    recipient character varying,
    diagnostic text,
    processed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.outreach_inbound_messages FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_inbound_messages_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.outreach_inbound_messages_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: outreach_inbound_messages_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.outreach_inbound_messages_id_seq OWNED BY public.outreach_inbound_messages.id;

--
-- Name: outreach_inbound_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_inbound_schedules (
    workspace_id character varying(64) NOT NULL,
    next_poll_at timestamp with time zone,
    enabled boolean DEFAULT false NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    uidvalidity character varying(64),
    last_uid character varying(64),
    updated_at timestamp with time zone DEFAULT now()
);

--
-- Name: outreach_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_schedules (
    sequence_id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    next_tick_at timestamp with time zone,
    enabled boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now()
);

--
-- Name: outreach_sends; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_sends (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    sequence_id character varying(36),
    enrollment_id integer,
    lead_id integer,
    step_number integer,
    to_email character varying NOT NULL,
    subject character varying,
    status character varying(20),
    skip_reason character varying(40),
    message_id character varying,
    idempotency_key character varying(255) NOT NULL,
    charged_usd double precision,
    migrated boolean DEFAULT false,
    error text,
    sent_at timestamp with time zone,
    opened_at timestamp with time zone,
    replied_at timestamp with time zone,
    bounced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.outreach_sends FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_sends_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.outreach_sends_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: outreach_sends_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.outreach_sends_id_seq OWNED BY public.outreach_sends.id;

--
-- Name: outreach_sequences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_sequences (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    name character varying(200) NOT NULL,
    description text,
    steps json NOT NULL,
    status character varying(20),
    daily_limit integer,
    send_window_start integer,
    send_window_end integer,
    send_window_tz character varying(40),
    consent_basis character varying(40) DEFAULT ''::character varying NOT NULL,
    bounce_count integer DEFAULT 0,
    complaint_count integer DEFAULT 0,
    auto_paused boolean DEFAULT false,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.outreach_sequences FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_suppressions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outreach_suppressions (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    email character varying NOT NULL,
    reason character varying(20),
    source character varying(40),
    locked boolean DEFAULT false,
    created_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.outreach_suppressions FORCE ROW LEVEL SECURITY;

--
-- Name: outreach_suppressions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.outreach_suppressions_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: outreach_suppressions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.outreach_suppressions_id_seq OWNED BY public.outreach_suppressions.id;

--
-- Name: person_employments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.person_employments (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    person_id character varying NOT NULL,
    company_key character varying(512) NOT NULL,
    company_entity_id character varying,
    company_name character varying(512),
    company_domain character varying(255),
    title character varying(512),
    titles json,
    source character varying(255),
    evidence_url character varying(1024),
    first_observed_at timestamp without time zone NOT NULL,
    last_observed_at timestamp without time zone NOT NULL,
    is_current integer DEFAULT 1 NOT NULL
);

ALTER TABLE ONLY public.person_employments FORCE ROW LEVEL SECURITY;

--
-- Name: person_employments_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.person_employments_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: person_employments_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.person_employments_id_seq OWNED BY public.person_employments.id;

--
-- Name: person_entities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.person_entities (
    id character varying NOT NULL,
    full_name character varying(255) NOT NULL,
    company_entity_id character varying,
    identity_keys json,
    fields json,
    corroboration_count integer,
    first_seen timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    last_seen timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.person_entities FORCE ROW LEVEL SECURITY;

--
-- Name: person_identifiers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.person_identifiers (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    kind character varying(32) NOT NULL,
    value character varying(512) NOT NULL,
    person_id character varying NOT NULL,
    created_at timestamp without time zone DEFAULT now()
);

ALTER TABLE ONLY public.person_identifiers FORCE ROW LEVEL SECURITY;

--
-- Name: person_identifiers_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.person_identifiers_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: person_identifiers_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.person_identifiers_id_seq OWNED BY public.person_identifiers.id;

--
-- Name: person_intel; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.person_intel (
    id integer NOT NULL,
    linkedin_url character varying,
    username character varying,
    name character varying,
    headline character varying,
    location character varying,
    summary text,
    emails json,
    social_links json,
    articles json,
    mentions json,
    companies json,
    education json,
    skills json,
    raw_sources json,
    status character varying,
    created_at timestamp without time zone,
    updated_at timestamp without time zone
);

--
-- Name: person_intel_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.person_intel_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: person_intel_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.person_intel_id_seq OWNED BY public.person_intel.id;

--
-- Name: playbook_results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.playbook_results (
    id integer NOT NULL,
    workspace_id character varying NOT NULL,
    run_id character varying NOT NULL,
    lead_id integer NOT NULL,
    status character varying(20) NOT NULL,
    value text NOT NULL,
    result_metadata json NOT NULL,
    error text,
    attempts integer NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.playbook_results FORCE ROW LEVEL SECURITY;

--
-- Name: playbook_results_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.playbook_results_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: playbook_results_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.playbook_results_id_seq OWNED BY public.playbook_results.id;

--
-- Name: playbook_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.playbook_runs (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    playbook_id character varying NOT NULL,
    audience_id character varying NOT NULL,
    status character varying(24) NOT NULL,
    prompt_version integer NOT NULL,
    prompt_snapshot text NOT NULL,
    max_members integer NOT NULL,
    attempted integer NOT NULL,
    succeeded integer NOT NULL,
    failed integer NOT NULL,
    error text,
    requested_by character varying,
    started_at timestamp without time zone,
    finished_at timestamp without time zone,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    steps_snapshot json DEFAULT '[]'::json NOT NULL
);

ALTER TABLE ONLY public.playbook_runs FORCE ROW LEVEL SECURITY;

--
-- Name: playbook_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.playbook_schedules (
    playbook_id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    next_run_at timestamp without time zone,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

--
-- Name: poll_budget_ledger; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.poll_budget_ledger (
    workspace_id character varying(64) NOT NULL,
    day date NOT NULL,
    n integer DEFAULT 0 NOT NULL,
    n_poll_now integer DEFAULT 0 NOT NULL
);

ALTER TABLE ONLY public.poll_budget_ledger FORCE ROW LEVEL SECURITY;

--
-- Name: provider_stats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.provider_stats (
    id integer NOT NULL,
    provider character varying(100) NOT NULL,
    field character varying(100) NOT NULL,
    attempts integer,
    hits integer,
    total_confidence double precision,
    total_latency_ms double precision,
    total_cost_usd double precision,
    cooldown_until timestamp without time zone,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    accuracy_score double precision,
    accuracy_samples integer,
    accuracy_updated_at timestamp without time zone
);

--
-- Name: provider_stats_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.provider_stats_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: provider_stats_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.provider_stats_id_seq OWNED BY public.provider_stats.id;

--
-- Name: research_playbooks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.research_playbooks (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    name character varying(200) NOT NULL,
    description text NOT NULL,
    prompt_template text NOT NULL,
    output_format character varying(20) NOT NULL,
    max_steps integer NOT NULL,
    cell_budget_usd double precision NOT NULL,
    version integer NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL,
    schedule_audience_id character varying,
    schedule_interval_minutes integer,
    next_run_at timestamp without time zone,
    steps json DEFAULT '[]'::json NOT NULL
);

ALTER TABLE ONLY public.research_playbooks FORCE ROW LEVEL SECURITY;

--
-- Name: retention_policies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.retention_policies (
    workspace_id character varying(64) NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    legal_hold boolean DEFAULT false NOT NULL,
    retention_days json NOT NULL,
    next_run_at timestamp without time zone,
    updated_by integer,
    created_at timestamp without time zone DEFAULT now() NOT NULL,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.retention_policies FORCE ROW LEVEL SECURITY;

--
-- Name: retention_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.retention_runs (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    status character varying(24) NOT NULL,
    requested_by character varying(64),
    policy_snapshot json NOT NULL,
    deleted_counts json NOT NULL,
    error text,
    started_at timestamp without time zone,
    finished_at timestamp without time zone,
    created_at timestamp without time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.retention_runs FORCE ROW LEVEL SECURITY;

--
-- Name: retention_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.retention_schedules (
    workspace_id character varying(64) NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    next_run_at timestamp without time zone,
    updated_at timestamp without time zone DEFAULT now() NOT NULL
);

--
-- Name: scheduled_triggers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scheduled_triggers (
    trigger_id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    next_run_at timestamp with time zone,
    enabled boolean DEFAULT true NOT NULL,
    updated_at timestamp with time zone DEFAULT now()
);

--
-- Name: scrape_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.scrape_history (
    id integer NOT NULL,
    url character varying,
    title character varying,
    method character varying,
    word_count integer,
    email_count integer,
    created_at character varying
);

--
-- Name: scrape_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.scrape_history_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: scrape_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.scrape_history_id_seq OWNED BY public.scrape_history.id;

--
-- Name: signals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.signals (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    lead_id integer,
    company character varying,
    signal_type character varying,
    title character varying,
    description text,
    source character varying,
    source_url character varying,
    weight integer,
    created_at double precision,
    read boolean
);

ALTER TABLE ONLY public.signals FORCE ROW LEVEL SECURITY;

--
-- Name: source_health; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.source_health (
    name character varying(100) NOT NULL,
    state character varying(20) DEFAULT 'healthy'::character varying NOT NULL,
    consecutive_zero integer DEFAULT 0 NOT NULL,
    consecutive_nonzero integer DEFAULT 0 NOT NULL,
    last_yield integer,
    ewma_yield double precision,
    probes integer DEFAULT 0 NOT NULL,
    last_probe_at timestamp without time zone,
    last_ok_at timestamp without time zone,
    last_outage_at timestamp without time zone,
    disabled_at timestamp without time zone,
    disabled_reason character varying(50),
    manual_override boolean DEFAULT false NOT NULL,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP
);

--
-- Name: source_stats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.source_stats (
    id integer NOT NULL,
    source character varying(100) NOT NULL,
    region character varying(50) NOT NULL,
    runs integer,
    runs_with_output integer,
    emitted integer,
    survived_dedup integer,
    validated integer,
    reliability double precision,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP
);

--
-- Name: source_stats_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.source_stats_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: source_stats_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.source_stats_id_seq OWNED BY public.source_stats.id;

--
-- Name: trigger_action_results; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.trigger_action_results (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    run_id character varying(36) NOT NULL,
    trigger_id character varying(36) NOT NULL,
    workbook_id character varying(36) NOT NULL,
    row_id character varying(36) NOT NULL,
    lead_id integer,
    action_index integer NOT NULL,
    action_type character varying(32) NOT NULL,
    idempotency_key character varying(255) NOT NULL,
    status character varying(20),
    skip_reason character varying(40),
    charged_usd double precision,
    result_summary text,
    error text,
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.trigger_action_results FORCE ROW LEVEL SECURITY;

--
-- Name: trigger_action_results_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.trigger_action_results_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: trigger_action_results_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.trigger_action_results_id_seq OWNED BY public.trigger_action_results.id;

--
-- Name: trigger_cap_reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.trigger_cap_reservations (
    id integer NOT NULL,
    workspace_id character varying(64) NOT NULL,
    trigger_id character varying(36) NOT NULL,
    day_utc character varying(10) NOT NULL,
    reserved_usd double precision NOT NULL,
    reserved_actions integer DEFAULT 1 NOT NULL,
    idempotency_key character varying(255) NOT NULL,
    state character varying(12) DEFAULT 'held'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.trigger_cap_reservations FORCE ROW LEVEL SECURITY;

--
-- Name: trigger_cap_reservations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.trigger_cap_reservations_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: trigger_cap_reservations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.trigger_cap_reservations_id_seq OWNED BY public.trigger_cap_reservations.id;

--
-- Name: trigger_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.trigger_runs (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    trigger_id character varying(36) NOT NULL,
    trigger_name_snapshot character varying(200),
    job_id integer,
    status character varying(20),
    fire_source character varying(32),
    fire_key character varying(200),
    sweep_cursor character varying(200),
    matched_rows integer,
    actions_attempted integer,
    actions_succeeded integer,
    actions_skipped integer,
    actions_failed integer,
    total_charged_usd double precision,
    error text,
    started_at timestamp with time zone DEFAULT now(),
    finished_at timestamp with time zone
);

ALTER TABLE ONLY public.trigger_runs FORCE ROW LEVEL SECURITY;

--
-- Name: triggers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.triggers (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    name character varying(200) NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    trigger_type character varying(32) NOT NULL,
    trigger_config json NOT NULL,
    condition text,
    actions json NOT NULL,
    scope_workbook_ids json,
    stop_on_error boolean DEFAULT false NOT NULL,
    max_spend_usd_per_day double precision,
    max_actions_per_day integer,
    next_run_at timestamp with time zone,
    schedule_anchor timestamp with time zone,
    last_fired_at timestamp with time zone,
    created_by character varying(64),
    created_at timestamp with time zone DEFAULT now(),
    updated_at timestamp with time zone DEFAULT now()
);

ALTER TABLE ONLY public.triggers FORCE ROW LEVEL SECURITY;

--
-- Name: user_active_workspace; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_active_workspace (
    user_id integer NOT NULL,
    workspace_id character varying(64) NOT NULL
);

--
-- Name: user_active_workspace_user_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.user_active_workspace_user_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: user_active_workspace_user_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.user_active_workspace_user_id_seq OWNED BY public.user_active_workspace.user_id;

--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id integer NOT NULL,
    username character varying,
    hashed_password character varying,
    is_active boolean,
    is_admin boolean,
    role character varying,
    profile_image character varying,
    last_login timestamp without time zone,
    created_at timestamp without time zone
);

--
-- Name: users_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.users_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: users_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.users_id_seq OWNED BY public.users.id;

--
-- Name: watch_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.watch_schedules (
    watch_id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    next_poll_at timestamp with time zone,
    enabled boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now()
);

--
-- Name: watch_subscriptions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.watch_subscriptions (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    kind character varying(20) NOT NULL,
    target character varying NOT NULL,
    resolved_cik character varying(10),
    lead_id integer,
    signal_types json,
    "interval" character varying(10) DEFAULT 'daily'::character varying,
    schedule_anchor timestamp with time zone,
    enabled boolean DEFAULT true NOT NULL,
    next_poll_at timestamp with time zone,
    cursor json,
    last_polled_at timestamp with time zone,
    last_error character varying,
    consecutive_failures integer DEFAULT 0,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now(),
    config json
);

ALTER TABLE ONLY public.watch_subscriptions FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_activity; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_activity (
    id integer NOT NULL,
    workbook_id character varying NOT NULL,
    kind character varying(50),
    message text,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.workbook_activity FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_activity_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.workbook_activity_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: workbook_activity_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.workbook_activity_id_seq OWNED BY public.workbook_activity.id;

--
-- Name: workbook_enrichments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_enrichments (
    id integer NOT NULL,
    workbook_id character varying NOT NULL,
    lead_id integer NOT NULL,
    column_id character varying(100) NOT NULL,
    value text,
    status character varying(50),
    provider character varying(100),
    error text,
    cell_metadata json,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL
);

ALTER TABLE ONLY public.workbook_enrichments FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_enrichments_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.workbook_enrichments_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: workbook_enrichments_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.workbook_enrichments_id_seq OWNED BY public.workbook_enrichments.id;

--
-- Name: workbook_functions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_functions (
    id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text,
    category text DEFAULT 'custom'::text,
    columns_chain jsonb DEFAULT '[]'::jsonb NOT NULL,
    version integer DEFAULT 1,
    usage_count integer DEFAULT 0,
    created_at double precision,
    updated_at double precision
);

--
-- Name: workbook_ingest_idempotency; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_ingest_idempotency (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    workbook_id character varying NOT NULL,
    idempotency_key character varying(128) NOT NULL,
    response json NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.workbook_ingest_idempotency FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_ingest_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_ingest_tokens (
    id character varying(36) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    workbook_id character varying NOT NULL,
    token_hash character varying(64) NOT NULL,
    prefix character varying(20) NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    created_by integer
);

--
-- Name: workbook_rows; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_rows (
    id integer NOT NULL,
    workbook_id character varying NOT NULL,
    "position" integer,
    data json,
    enrichments json,
    lead_id integer,
    canonical_entity_id character varying,
    corroboration_count integer,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    workspace_id character varying NOT NULL,
    source_provider character varying(100),
    source_record_id character varying(255),
    source_rank integer,
    source_fetched_at timestamp with time zone
);

ALTER TABLE ONLY public.workbook_rows FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_rows_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.workbook_rows_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: workbook_rows_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.workbook_rows_id_seq OWNED BY public.workbook_rows.id;

--
-- Name: workbook_spend_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_spend_attempts (
    id character varying(36) NOT NULL,
    workspace_id character varying(255) NOT NULL,
    workbook_id character varying(36) NOT NULL,
    run_id character varying(255) NOT NULL,
    row_identity character varying(255) NOT NULL,
    column_id character varying(255) NOT NULL,
    provider character varying(255) NOT NULL,
    attempt_key character varying(255) NOT NULL,
    contract_hash character varying(64) NOT NULL,
    status character varying(20) NOT NULL,
    reserved_microusd bigint NOT NULL,
    settled_microusd bigint,
    cost_basis json NOT NULL,
    created_at double precision NOT NULL,
    updated_at double precision NOT NULL,
    result json,
    CONSTRAINT ck_workbook_spend_nonnegative CHECK ((reserved_microusd >= 0)),
    CONSTRAINT ck_workbook_spend_settlement_nonnegative CHECK (((settled_microusd IS NULL) OR (settled_microusd >= 0))),
    CONSTRAINT ck_workbook_spend_status CHECK (((status)::text = ANY (ARRAY[('reserved'::character varying)::text, ('dispatched'::character varying)::text, ('settled'::character varying)::text, ('released'::character varying)::text, ('uncertain'::character varying)::text])))
);

ALTER TABLE ONLY public.workbook_spend_attempts FORCE ROW LEVEL SECURITY;

--
-- Name: workbook_views; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbook_views (
    id character varying NOT NULL,
    workspace_id character varying NOT NULL,
    workbook_id character varying NOT NULL,
    name character varying(255) NOT NULL,
    config json,
    created_at timestamp without time zone DEFAULT now(),
    updated_at timestamp without time zone DEFAULT now()
);

ALTER TABLE ONLY public.workbook_views FORCE ROW LEVEL SECURITY;

--
-- Name: workbooks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workbooks (
    id character varying NOT NULL,
    name character varying(255) NOT NULL,
    description text,
    status character varying(50),
    workspace_id character varying NOT NULL,
    source_type character varying(50),
    source_config json,
    filter_criteria json,
    columns_config json,
    total_rows integer,
    completed_rows integer,
    sync_to_leads boolean,
    budget_max_usd double precision,
    budget_spent_usd double precision,
    refresh_policy json,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    last_run_at timestamp without time zone,
    action_idempotency_key character varying(255)
);

ALTER TABLE ONLY public.workbooks FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_credits; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_credits (
    workspace_id character varying(64) NOT NULL,
    balance_usd double precision NOT NULL,
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE ONLY public.workspace_credits FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_member_permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_member_permissions (
    workspace_id character varying(64) NOT NULL,
    user_id integer NOT NULL,
    permission text NOT NULL,
    effect text NOT NULL,
    updated_at double precision NOT NULL,
    CONSTRAINT ck_wmp_effect CHECK ((effect = ANY (ARRAY['allow'::text, 'deny'::text])))
);

ALTER TABLE ONLY public.workspace_member_permissions FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_members (
    workspace_id character varying(64) NOT NULL,
    user_id integer NOT NULL,
    role text DEFAULT 'member'::text NOT NULL,
    created_at double precision
);

--
-- Name: workspace_oidc_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_oidc_identities (
    workspace_id character varying(64) NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    user_id integer NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    created_at double precision NOT NULL
);

--
-- Name: workspace_scim_group_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_scim_group_members (
    workspace_id character varying(64) NOT NULL,
    group_id character varying(64) NOT NULL,
    user_id integer NOT NULL,
    created_at double precision NOT NULL
);

ALTER TABLE ONLY public.workspace_scim_group_members FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_scim_groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_scim_groups (
    id character varying(64) NOT NULL,
    workspace_id character varying(64) NOT NULL,
    external_id text DEFAULT ''::text NOT NULL,
    display_name text NOT NULL,
    created_at double precision NOT NULL,
    updated_at double precision NOT NULL
);

ALTER TABLE ONLY public.workspace_scim_groups FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_scim_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_scim_tokens (
    workspace_id character varying(64) NOT NULL,
    token_hash text NOT NULL,
    token_prefix text NOT NULL,
    created_at double precision NOT NULL,
    expires_at double precision,
    last_used_at double precision,
    created_by integer
);

--
-- Name: workspace_scim_users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_scim_users (
    workspace_id character varying(64) NOT NULL,
    user_id integer NOT NULL,
    external_id text DEFAULT ''::text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at double precision NOT NULL,
    updated_at double precision NOT NULL
);

ALTER TABLE ONLY public.workspace_scim_users FORCE ROW LEVEL SECURITY;

--
-- Name: workspace_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspace_settings (
    workspace_id character varying(64) NOT NULL,
    key text NOT NULL,
    value text DEFAULT ''::text NOT NULL
);

ALTER TABLE ONLY public.workspace_settings FORCE ROW LEVEL SECURITY;

--
-- Name: workspaces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.workspaces (
    id character varying(64) NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    icon text DEFAULT '🏢'::text NOT NULL,
    owner_id integer,
    created_at double precision,
    updated_at double precision
);

--
-- Name: audience_members id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_members ALTER COLUMN id SET DEFAULT nextval('public.audience_members_id_seq'::regclass);

--
-- Name: audience_membership_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_membership_events ALTER COLUMN id SET DEFAULT nextval('public.audience_membership_events_id_seq'::regclass);

--
-- Name: cell_traces id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cell_traces ALTER COLUMN id SET DEFAULT nextval('public.cell_traces_id_seq'::regclass);

--
-- Name: chat_memories id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_memories ALTER COLUMN id SET DEFAULT nextval('public.chat_memories_id_seq'::regclass);

--
-- Name: collection_job_stages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_job_stages ALTER COLUMN id SET DEFAULT nextval('public.collection_job_stages_id_seq'::regclass);

--
-- Name: company_identifiers id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.company_identifiers ALTER COLUMN id SET DEFAULT nextval('public.company_identifiers_id_seq'::regclass);

--
-- Name: credit_ledger_entries id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger_entries ALTER COLUMN id SET DEFAULT nextval('public.credit_ledger_entries_id_seq'::regclass);

--
-- Name: destination_deliveries id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_deliveries ALTER COLUMN id SET DEFAULT nextval('public.destination_deliveries_id_seq'::regclass);

--
-- Name: email_data id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_data ALTER COLUMN id SET DEFAULT nextval('public.email_data_id_seq'::regclass);

--
-- Name: entity_blocking_keys id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_blocking_keys ALTER COLUMN id SET DEFAULT nextval('public.entity_blocking_keys_id_seq'::regclass);

--
-- Name: entity_merge_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_merge_log ALTER COLUMN id SET DEFAULT nextval('public.entity_merge_log_id_seq'::regclass);

--
-- Name: entity_review_pairs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_review_pairs ALTER COLUMN id SET DEFAULT nextval('public.entity_review_pairs_id_seq'::regclass);

--
-- Name: jobs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs ALTER COLUMN id SET DEFAULT nextval('public.jobs_id_seq'::regclass);

--
-- Name: leads id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leads ALTER COLUMN id SET DEFAULT nextval('public.leads_id_seq'::regclass);

--
-- Name: links id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.links ALTER COLUMN id SET DEFAULT nextval('public.links_id_seq'::regclass);

--
-- Name: llm_usage_daily id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.llm_usage_daily ALTER COLUMN id SET DEFAULT nextval('public.llm_usage_daily_id_seq'::regclass);

--
-- Name: outreach_enrollments id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_enrollments ALTER COLUMN id SET DEFAULT nextval('public.outreach_enrollments_id_seq'::regclass);

--
-- Name: outreach_inbound_messages id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_inbound_messages ALTER COLUMN id SET DEFAULT nextval('public.outreach_inbound_messages_id_seq'::regclass);

--
-- Name: outreach_sends id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_sends ALTER COLUMN id SET DEFAULT nextval('public.outreach_sends_id_seq'::regclass);

--
-- Name: outreach_suppressions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_suppressions ALTER COLUMN id SET DEFAULT nextval('public.outreach_suppressions_id_seq'::regclass);

--
-- Name: person_employments id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_employments ALTER COLUMN id SET DEFAULT nextval('public.person_employments_id_seq'::regclass);

--
-- Name: person_identifiers id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_identifiers ALTER COLUMN id SET DEFAULT nextval('public.person_identifiers_id_seq'::regclass);

--
-- Name: person_intel id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_intel ALTER COLUMN id SET DEFAULT nextval('public.person_intel_id_seq'::regclass);

--
-- Name: playbook_results id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_results ALTER COLUMN id SET DEFAULT nextval('public.playbook_results_id_seq'::regclass);

--
-- Name: provider_stats id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_stats ALTER COLUMN id SET DEFAULT nextval('public.provider_stats_id_seq'::regclass);

--
-- Name: scrape_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scrape_history ALTER COLUMN id SET DEFAULT nextval('public.scrape_history_id_seq'::regclass);

--
-- Name: source_stats id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.source_stats ALTER COLUMN id SET DEFAULT nextval('public.source_stats_id_seq'::regclass);

--
-- Name: trigger_action_results id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_action_results ALTER COLUMN id SET DEFAULT nextval('public.trigger_action_results_id_seq'::regclass);

--
-- Name: trigger_cap_reservations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_cap_reservations ALTER COLUMN id SET DEFAULT nextval('public.trigger_cap_reservations_id_seq'::regclass);

--
-- Name: user_active_workspace user_id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_active_workspace ALTER COLUMN user_id SET DEFAULT nextval('public.user_active_workspace_user_id_seq'::regclass);

--
-- Name: users id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users ALTER COLUMN id SET DEFAULT nextval('public.users_id_seq'::regclass);

--
-- Name: workbook_activity id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_activity ALTER COLUMN id SET DEFAULT nextval('public.workbook_activity_id_seq'::regclass);

--
-- Name: workbook_enrichments id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_enrichments ALTER COLUMN id SET DEFAULT nextval('public.workbook_enrichments_id_seq'::regclass);

--
-- Name: workbook_rows id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_rows ALTER COLUMN id SET DEFAULT nextval('public.workbook_rows_id_seq'::regclass);

--
-- Name: alembic_version alembic_version_pkc; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.alembic_version
    ADD CONSTRAINT alembic_version_pkc PRIMARY KEY (version_num);

--
-- Name: app_settings app_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_settings
    ADD CONSTRAINT app_settings_pkey PRIMARY KEY (key);

--
-- Name: audience_destinations audience_destinations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_destinations
    ADD CONSTRAINT audience_destinations_pkey PRIMARY KEY (id);

--
-- Name: audience_members audience_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_members
    ADD CONSTRAINT audience_members_pkey PRIMARY KEY (id);

--
-- Name: audience_membership_events audience_membership_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_membership_events
    ADD CONSTRAINT audience_membership_events_pkey PRIMARY KEY (id);

--
-- Name: audience_schedules audience_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_schedules
    ADD CONSTRAINT audience_schedules_pkey PRIMARY KEY (audience_id);

--
-- Name: audiences audiences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audiences
    ADD CONSTRAINT audiences_pkey PRIMARY KEY (id);

--
-- Name: cell_traces cell_traces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cell_traces
    ADD CONSTRAINT cell_traces_pkey PRIMARY KEY (id);

--
-- Name: chat_conversations chat_conversations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_conversations
    ADD CONSTRAINT chat_conversations_pkey PRIMARY KEY (id);

--
-- Name: chat_memories chat_memories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_memories
    ADD CONSTRAINT chat_memories_pkey PRIMARY KEY (id);

--
-- Name: chat_messages chat_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages
    ADD CONSTRAINT chat_messages_pkey PRIMARY KEY (id);

--
-- Name: chat_tool_approvals chat_tool_approvals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_tool_approvals
    ADD CONSTRAINT chat_tool_approvals_pkey PRIMARY KEY (id);

--
-- Name: collection_job_stages collection_job_stages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_job_stages
    ADD CONSTRAINT collection_job_stages_pkey PRIMARY KEY (id);

--
-- Name: collection_jobs collection_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_jobs
    ADD CONSTRAINT collection_jobs_pkey PRIMARY KEY (id);

--
-- Name: company_entities company_entities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.company_entities
    ADD CONSTRAINT company_entities_pkey PRIMARY KEY (id);

--
-- Name: company_identifiers company_identifiers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.company_identifiers
    ADD CONSTRAINT company_identifiers_pkey PRIMARY KEY (id);

--
-- Name: connector_runs connector_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.connector_runs
    ADD CONSTRAINT connector_runs_pkey PRIMARY KEY (id);

--
-- Name: contact_executions contact_executions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_executions
    ADD CONSTRAINT contact_executions_pkey PRIMARY KEY (id);

--
-- Name: credit_ledger_entries credit_ledger_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger_entries
    ADD CONSTRAINT credit_ledger_entries_pkey PRIMARY KEY (id);

--
-- Name: destination_deliveries destination_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_deliveries
    ADD CONSTRAINT destination_deliveries_pkey PRIMARY KEY (id);

--
-- Name: destination_inbound_receipts destination_inbound_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_inbound_receipts
    ADD CONSTRAINT destination_inbound_receipts_pkey PRIMARY KEY (id);

--
-- Name: destination_inbound_tokens destination_inbound_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_inbound_tokens
    ADD CONSTRAINT destination_inbound_tokens_pkey PRIMARY KEY (id);

--
-- Name: destination_runs destination_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_runs
    ADD CONSTRAINT destination_runs_pkey PRIMARY KEY (id);

--
-- Name: email_data email_data_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_data
    ADD CONSTRAINT email_data_pkey PRIMARY KEY (id);

--
-- Name: email_verify_cache email_verify_cache_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.email_verify_cache
    ADD CONSTRAINT email_verify_cache_pkey PRIMARY KEY (workspace_id, email_hash);

--
-- Name: enrichment_cache enrichment_cache_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.enrichment_cache
    ADD CONSTRAINT enrichment_cache_pkey PRIMARY KEY (workspace_id, cache_key, field_name);

--
-- Name: entity_blocking_keys entity_blocking_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_blocking_keys
    ADD CONSTRAINT entity_blocking_keys_pkey PRIMARY KEY (id);

--
-- Name: entity_merge_log entity_merge_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_merge_log
    ADD CONSTRAINT entity_merge_log_pkey PRIMARY KEY (id);

--
-- Name: entity_review_pairs entity_review_pairs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_review_pairs
    ADD CONSTRAINT entity_review_pairs_pkey PRIMARY KEY (id);

--
-- Name: governance_audit_events governance_audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.governance_audit_events
    ADD CONSTRAINT governance_audit_events_pkey PRIMARY KEY (id);

--
-- Name: jobs jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);

--
-- Name: leads leads_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leads
    ADD CONSTRAINT leads_pkey PRIMARY KEY (id);

--
-- Name: links links_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.links
    ADD CONSTRAINT links_pkey PRIMARY KEY (id);

--
-- Name: llm_usage_daily llm_usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.llm_usage_daily
    ADD CONSTRAINT llm_usage_daily_pkey PRIMARY KEY (id);

--
-- Name: mcp_audit_log mcp_audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_audit_log
    ADD CONSTRAINT mcp_audit_log_pkey PRIMARY KEY (id);

--
-- Name: mcp_tokens mcp_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_tokens
    ADD CONSTRAINT mcp_tokens_pkey PRIMARY KEY (id);

--
-- Name: outreach_drafts outreach_drafts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_drafts
    ADD CONSTRAINT outreach_drafts_pkey PRIMARY KEY (id);

--
-- Name: outreach_enrollments outreach_enrollments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_enrollments
    ADD CONSTRAINT outreach_enrollments_pkey PRIMARY KEY (id);

--
-- Name: outreach_inbound_messages outreach_inbound_messages_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_inbound_messages
    ADD CONSTRAINT outreach_inbound_messages_pkey PRIMARY KEY (id);

--
-- Name: outreach_inbound_schedules outreach_inbound_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_inbound_schedules
    ADD CONSTRAINT outreach_inbound_schedules_pkey PRIMARY KEY (workspace_id);

--
-- Name: outreach_schedules outreach_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_schedules
    ADD CONSTRAINT outreach_schedules_pkey PRIMARY KEY (sequence_id);

--
-- Name: outreach_sends outreach_sends_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_sends
    ADD CONSTRAINT outreach_sends_pkey PRIMARY KEY (id);

--
-- Name: outreach_sequences outreach_sequences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_sequences
    ADD CONSTRAINT outreach_sequences_pkey PRIMARY KEY (id);

--
-- Name: outreach_suppressions outreach_suppressions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_suppressions
    ADD CONSTRAINT outreach_suppressions_pkey PRIMARY KEY (id);

--
-- Name: person_employments person_employments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_employments
    ADD CONSTRAINT person_employments_pkey PRIMARY KEY (id);

--
-- Name: person_entities person_entities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_entities
    ADD CONSTRAINT person_entities_pkey PRIMARY KEY (id);

--
-- Name: person_identifiers person_identifiers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_identifiers
    ADD CONSTRAINT person_identifiers_pkey PRIMARY KEY (id);

--
-- Name: person_intel person_intel_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_intel
    ADD CONSTRAINT person_intel_pkey PRIMARY KEY (id);

--
-- Name: playbook_results playbook_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_results
    ADD CONSTRAINT playbook_results_pkey PRIMARY KEY (id);

--
-- Name: playbook_runs playbook_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_runs
    ADD CONSTRAINT playbook_runs_pkey PRIMARY KEY (id);

--
-- Name: playbook_schedules playbook_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_schedules
    ADD CONSTRAINT playbook_schedules_pkey PRIMARY KEY (playbook_id);

--
-- Name: poll_budget_ledger poll_budget_ledger_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.poll_budget_ledger
    ADD CONSTRAINT poll_budget_ledger_pkey PRIMARY KEY (workspace_id, day);

--
-- Name: provider_stats provider_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_stats
    ADD CONSTRAINT provider_stats_pkey PRIMARY KEY (id);

--
-- Name: research_playbooks research_playbooks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_playbooks
    ADD CONSTRAINT research_playbooks_pkey PRIMARY KEY (id);

--
-- Name: retention_policies retention_policies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.retention_policies
    ADD CONSTRAINT retention_policies_pkey PRIMARY KEY (workspace_id);

--
-- Name: retention_runs retention_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.retention_runs
    ADD CONSTRAINT retention_runs_pkey PRIMARY KEY (id);

--
-- Name: retention_schedules retention_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.retention_schedules
    ADD CONSTRAINT retention_schedules_pkey PRIMARY KEY (workspace_id);

--
-- Name: scheduled_triggers scheduled_triggers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scheduled_triggers
    ADD CONSTRAINT scheduled_triggers_pkey PRIMARY KEY (trigger_id);

--
-- Name: scrape_history scrape_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.scrape_history
    ADD CONSTRAINT scrape_history_pkey PRIMARY KEY (id);

--
-- Name: signals signals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.signals
    ADD CONSTRAINT signals_pkey PRIMARY KEY (id);

--
-- Name: source_health source_health_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.source_health
    ADD CONSTRAINT source_health_pkey PRIMARY KEY (name);

--
-- Name: source_stats source_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.source_stats
    ADD CONSTRAINT source_stats_pkey PRIMARY KEY (id);

--
-- Name: trigger_action_results trigger_action_results_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_action_results
    ADD CONSTRAINT trigger_action_results_pkey PRIMARY KEY (id);

--
-- Name: trigger_cap_reservations trigger_cap_reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_cap_reservations
    ADD CONSTRAINT trigger_cap_reservations_pkey PRIMARY KEY (id);

--
-- Name: trigger_runs trigger_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_runs
    ADD CONSTRAINT trigger_runs_pkey PRIMARY KEY (id);

--
-- Name: triggers triggers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.triggers
    ADD CONSTRAINT triggers_pkey PRIMARY KEY (id);

--
-- Name: audience_destinations uq_audience_destinations_name; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_destinations
    ADD CONSTRAINT uq_audience_destinations_name UNIQUE (workspace_id, audience_id, name);

--
-- Name: audience_members uq_audience_members_audience_lead; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_members
    ADD CONSTRAINT uq_audience_members_audience_lead UNIQUE (audience_id, lead_id);

--
-- Name: audiences uq_audiences_workspace_name; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audiences
    ADD CONSTRAINT uq_audiences_workspace_name UNIQUE (workspace_id, name);

--
-- Name: trigger_cap_reservations uq_cap_reservation_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_cap_reservations
    ADD CONSTRAINT uq_cap_reservation_idem UNIQUE (workspace_id, idempotency_key);

--
-- Name: cell_traces uq_cell_trace; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cell_traces
    ADD CONSTRAINT uq_cell_trace UNIQUE (workbook_id, lead_id, column_id);

--
-- Name: company_identifiers uq_company_identifier; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.company_identifiers
    ADD CONSTRAINT uq_company_identifier UNIQUE (workspace_id, kind, value);

--
-- Name: contact_executions uq_contact_execution_action; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.contact_executions
    ADD CONSTRAINT uq_contact_execution_action UNIQUE (workspace_id, action_id);

--
-- Name: destination_deliveries uq_destination_delivery_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_deliveries
    ADD CONSTRAINT uq_destination_delivery_idem UNIQUE (workspace_id, idempotency_key);

--
-- Name: destination_inbound_receipts uq_destination_inbound_event; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_inbound_receipts
    ADD CONSTRAINT uq_destination_inbound_event UNIQUE (workspace_id, destination_id, external_event_id);

--
-- Name: workbook_enrichments uq_enrichment_cell; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_enrichments
    ADD CONSTRAINT uq_enrichment_cell UNIQUE (workbook_id, lead_id, column_id);

--
-- Name: outreach_enrollments uq_enroll_ws_seq_lead; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_enrollments
    ADD CONSTRAINT uq_enroll_ws_seq_lead UNIQUE (workspace_id, sequence_id, lead_id);

--
-- Name: outreach_inbound_messages uq_inbound_ws_srcmid; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_inbound_messages
    ADD CONSTRAINT uq_inbound_ws_srcmid UNIQUE (workspace_id, source_message_id);

--
-- Name: outreach_inbound_messages uq_inbound_ws_uid; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_inbound_messages
    ADD CONSTRAINT uq_inbound_ws_uid UNIQUE (workspace_id, uidvalidity, imap_uid);

--
-- Name: leads uq_leads_ws_company_city; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leads
    ADD CONSTRAINT uq_leads_ws_company_city UNIQUE (workspace_id, company, city);

--
-- Name: credit_ledger_entries uq_ledger_idempotency_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credit_ledger_entries
    ADD CONSTRAINT uq_ledger_idempotency_key UNIQUE (idempotency_key);

--
-- Name: llm_usage_daily uq_llm_usage_ws_provider_date; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.llm_usage_daily
    ADD CONSTRAINT uq_llm_usage_ws_provider_date UNIQUE (workspace_id, provider, date);

--
-- Name: mcp_audit_log uq_mcp_audit_ws_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_audit_log
    ADD CONSTRAINT uq_mcp_audit_ws_idem UNIQUE (workspace_id, idempotency_key);

--
-- Name: outreach_drafts uq_outreach_draft_ws_action; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_drafts
    ADD CONSTRAINT uq_outreach_draft_ws_action UNIQUE (workspace_id, action_idempotency_key);

--
-- Name: outreach_sends uq_outreach_send_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_sends
    ADD CONSTRAINT uq_outreach_send_idem UNIQUE (workspace_id, idempotency_key);

--
-- Name: person_employments uq_person_employment; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_employments
    ADD CONSTRAINT uq_person_employment UNIQUE (workspace_id, person_id, company_key);

--
-- Name: person_identifiers uq_person_identifier; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_identifiers
    ADD CONSTRAINT uq_person_identifier UNIQUE (workspace_id, kind, value);

--
-- Name: playbook_results uq_playbook_result_run_lead; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_results
    ADD CONSTRAINT uq_playbook_result_run_lead UNIQUE (workspace_id, run_id, lead_id);

--
-- Name: provider_stats uq_provider_field; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_stats
    ADD CONSTRAINT uq_provider_field UNIQUE (provider, field);

--
-- Name: research_playbooks uq_research_playbook_name; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_playbooks
    ADD CONSTRAINT uq_research_playbook_name UNIQUE (workspace_id, name);

--
-- Name: source_stats uq_source_region; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.source_stats
    ADD CONSTRAINT uq_source_region UNIQUE (source, region);

--
-- Name: outreach_suppressions uq_suppress_ws_email; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outreach_suppressions
    ADD CONSTRAINT uq_suppress_ws_email UNIQUE (workspace_id, email);

--
-- Name: trigger_action_results uq_trigger_action_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trigger_action_results
    ADD CONSTRAINT uq_trigger_action_idem UNIQUE (workspace_id, idempotency_key);

--
-- Name: watch_subscriptions uq_watch_ws_kind_target; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.watch_subscriptions
    ADD CONSTRAINT uq_watch_ws_kind_target UNIQUE (workspace_id, kind, target);

--
-- Name: workbook_ingest_idempotency uq_wb_ingest_idem; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_ingest_idempotency
    ADD CONSTRAINT uq_wb_ingest_idem UNIQUE (workbook_id, idempotency_key);

--
-- Name: workbook_rows uq_workbook_row_source_identity; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_rows
    ADD CONSTRAINT uq_workbook_row_source_identity UNIQUE (workbook_id, source_provider, source_record_id);

--
-- Name: workbook_spend_attempts uq_workbook_spend_attempt_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_spend_attempts
    ADD CONSTRAINT uq_workbook_spend_attempt_key UNIQUE (workspace_id, attempt_key);

--
-- Name: workbooks uq_workbooks_workspace_action_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbooks
    ADD CONSTRAINT uq_workbooks_workspace_action_key UNIQUE (workspace_id, action_idempotency_key);

--
-- Name: user_active_workspace user_active_workspace_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_active_workspace
    ADD CONSTRAINT user_active_workspace_pkey PRIMARY KEY (user_id);

--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

--
-- Name: watch_schedules watch_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.watch_schedules
    ADD CONSTRAINT watch_schedules_pkey PRIMARY KEY (watch_id);

--
-- Name: watch_subscriptions watch_subscriptions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.watch_subscriptions
    ADD CONSTRAINT watch_subscriptions_pkey PRIMARY KEY (id);

--
-- Name: workbook_activity workbook_activity_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_activity
    ADD CONSTRAINT workbook_activity_pkey PRIMARY KEY (id);

--
-- Name: workbook_enrichments workbook_enrichments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_enrichments
    ADD CONSTRAINT workbook_enrichments_pkey PRIMARY KEY (id);

--
-- Name: workbook_functions workbook_functions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_functions
    ADD CONSTRAINT workbook_functions_pkey PRIMARY KEY (id);

--
-- Name: workbook_ingest_idempotency workbook_ingest_idempotency_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_ingest_idempotency
    ADD CONSTRAINT workbook_ingest_idempotency_pkey PRIMARY KEY (id);

--
-- Name: workbook_ingest_tokens workbook_ingest_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_ingest_tokens
    ADD CONSTRAINT workbook_ingest_tokens_pkey PRIMARY KEY (id);

--
-- Name: workbook_rows workbook_rows_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_rows
    ADD CONSTRAINT workbook_rows_pkey PRIMARY KEY (id);

--
-- Name: workbook_spend_attempts workbook_spend_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_spend_attempts
    ADD CONSTRAINT workbook_spend_attempts_pkey PRIMARY KEY (id);

--
-- Name: workbook_views workbook_views_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_views
    ADD CONSTRAINT workbook_views_pkey PRIMARY KEY (id);

--
-- Name: workbooks workbooks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbooks
    ADD CONSTRAINT workbooks_pkey PRIMARY KEY (id);

--
-- Name: workspace_credits workspace_credits_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_credits
    ADD CONSTRAINT workspace_credits_pkey PRIMARY KEY (workspace_id);

--
-- Name: workspace_member_permissions workspace_member_permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_member_permissions
    ADD CONSTRAINT workspace_member_permissions_pkey PRIMARY KEY (workspace_id, user_id, permission);

--
-- Name: workspace_members workspace_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_members
    ADD CONSTRAINT workspace_members_pkey PRIMARY KEY (workspace_id, user_id);

--
-- Name: workspace_oidc_identities workspace_oidc_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_oidc_identities
    ADD CONSTRAINT workspace_oidc_identities_pkey PRIMARY KEY (workspace_id, issuer, subject);

--
-- Name: workspace_oidc_identities workspace_oidc_identities_workspace_id_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_oidc_identities
    ADD CONSTRAINT workspace_oidc_identities_workspace_id_user_id_key UNIQUE (workspace_id, user_id);

--
-- Name: workspace_scim_group_members workspace_scim_group_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_group_members
    ADD CONSTRAINT workspace_scim_group_members_pkey PRIMARY KEY (workspace_id, group_id, user_id);

--
-- Name: workspace_scim_groups workspace_scim_groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_groups
    ADD CONSTRAINT workspace_scim_groups_pkey PRIMARY KEY (id);

--
-- Name: workspace_scim_groups workspace_scim_groups_workspace_id_display_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_groups
    ADD CONSTRAINT workspace_scim_groups_workspace_id_display_name_key UNIQUE (workspace_id, display_name);

--
-- Name: workspace_scim_tokens workspace_scim_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_tokens
    ADD CONSTRAINT workspace_scim_tokens_pkey PRIMARY KEY (workspace_id);

--
-- Name: workspace_scim_users workspace_scim_users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_users
    ADD CONSTRAINT workspace_scim_users_pkey PRIMARY KEY (workspace_id, user_id);

--
-- Name: workspace_settings workspace_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_settings
    ADD CONSTRAINT workspace_settings_pkey PRIMARY KEY (workspace_id, key);

--
-- Name: workspaces workspaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

--
-- Name: workspaces workspaces_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_slug_key UNIQUE (slug);

--
-- Name: idx_chat_conv_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chat_conv_tenant ON public.chat_conversations USING btree (workspace_id, user_id, updated_at);

--
-- Name: idx_chat_messages_conv; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chat_messages_conv ON public.chat_messages USING btree (conversation_id, created_at);

--
-- Name: idx_chat_tool_approvals_tenant; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_chat_tool_approvals_tenant ON public.chat_tool_approvals USING btree (workspace_id, user_id, created_at_epoch);

--
-- Name: ix_audience_destinations_audience_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_destinations_audience_id ON public.audience_destinations USING btree (audience_id);

--
-- Name: ix_audience_destinations_workspace_audience; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_destinations_workspace_audience ON public.audience_destinations USING btree (workspace_id, audience_id);

--
-- Name: ix_audience_destinations_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_destinations_workspace_id ON public.audience_destinations USING btree (workspace_id);

--
-- Name: ix_audience_events_refresh_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_events_refresh_cursor ON public.audience_membership_events USING btree (workspace_id, audience_id, refresh_id, id);

--
-- Name: ix_audience_events_workspace_audience_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_events_workspace_audience_created ON public.audience_membership_events USING btree (workspace_id, audience_id, created_at);

--
-- Name: ix_audience_events_workspace_audience_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_events_workspace_audience_cursor ON public.audience_membership_events USING btree (workspace_id, audience_id, id);

--
-- Name: ix_audience_members_audience_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_audience_id ON public.audience_members USING btree (audience_id);

--
-- Name: ix_audience_members_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_lead_id ON public.audience_members USING btree (lead_id);

--
-- Name: ix_audience_members_refresh_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_refresh_cursor ON public.audience_members USING btree (workspace_id, audience_id, refresh_token, id);

--
-- Name: ix_audience_members_workspace_audience; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_workspace_audience ON public.audience_members USING btree (workspace_id, audience_id);

--
-- Name: ix_audience_members_workspace_audience_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_workspace_audience_cursor ON public.audience_members USING btree (workspace_id, audience_id, id);

--
-- Name: ix_audience_members_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_members_workspace_id ON public.audience_members USING btree (workspace_id);

--
-- Name: ix_audience_membership_events_audience_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_membership_events_audience_id ON public.audience_membership_events USING btree (audience_id);

--
-- Name: ix_audience_membership_events_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_membership_events_lead_id ON public.audience_membership_events USING btree (lead_id);

--
-- Name: ix_audience_membership_events_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_membership_events_workspace_id ON public.audience_membership_events USING btree (workspace_id);

--
-- Name: ix_audience_schedules_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audience_schedules_due ON public.audience_schedules USING btree (enabled, next_refresh_at);

--
-- Name: ix_audiences_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audiences_workspace_id ON public.audiences USING btree (workspace_id);

--
-- Name: ix_audiences_workspace_updated; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_audiences_workspace_updated ON public.audiences USING btree (workspace_id, updated_at);

--
-- Name: ix_capres_ws_day; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_capres_ws_day ON public.trigger_cap_reservations USING btree (workspace_id, day_utc, state);

--
-- Name: ix_capres_ws_trig_day; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_capres_ws_trig_day ON public.trigger_cap_reservations USING btree (workspace_id, trigger_id, day_utc, state);

--
-- Name: ix_cell_traces_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_cell_traces_lead_id ON public.cell_traces USING btree (lead_id);

--
-- Name: ix_cell_traces_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_cell_traces_workbook_id ON public.cell_traces USING btree (workbook_id);

--
-- Name: ix_cell_traces_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_cell_traces_workspace_id ON public.cell_traces USING btree (workspace_id);

--
-- Name: ix_chat_memories_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_chat_memories_user ON public.chat_memories USING btree (user_id);

--
-- Name: ix_collection_job_stages_job; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_collection_job_stages_job ON public.collection_job_stages USING btree (job_id);

--
-- Name: ix_collection_jobs_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_collection_jobs_ws_created ON public.collection_jobs USING btree (workspace_id, created_at DESC);

--
-- Name: ix_collection_jobs_ws_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_collection_jobs_ws_status ON public.collection_jobs USING btree (workspace_id, status);

--
-- Name: ix_company_entities_corroboration_count; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_company_entities_corroboration_count ON public.company_entities USING btree (corroboration_count);

--
-- Name: ix_company_entities_primary_domain; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_company_entities_primary_domain ON public.company_entities USING btree (primary_domain);

--
-- Name: ix_company_entities_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_company_entities_workspace_id ON public.company_entities USING btree (workspace_id);

--
-- Name: ix_company_identifiers_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_company_identifiers_entity_id ON public.company_identifiers USING btree (entity_id);

--
-- Name: ix_company_identifiers_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_company_identifiers_workspace_id ON public.company_identifiers USING btree (workspace_id);

--
-- Name: ix_connector_runs_connector; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_connector_runs_connector ON public.connector_runs USING btree (connector);

--
-- Name: ix_connector_runs_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_connector_runs_status ON public.connector_runs USING btree (status);

--
-- Name: ix_connector_runs_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_connector_runs_workbook_id ON public.connector_runs USING btree (workbook_id);

--
-- Name: ix_connector_runs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_connector_runs_workspace_id ON public.connector_runs USING btree (workspace_id);

--
-- Name: ix_connector_runs_ws_workbook_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_connector_runs_ws_workbook_cursor ON public.connector_runs USING btree (workspace_id, workbook_id, created_at, id);

--
-- Name: ix_credit_ledger_entries_run_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_credit_ledger_entries_run_id ON public.credit_ledger_entries USING btree (run_id);

--
-- Name: ix_credit_ledger_entries_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_credit_ledger_entries_workspace_id ON public.credit_ledger_entries USING btree (workspace_id);

--
-- Name: ix_destination_deliveries_destination_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_deliveries_destination_id ON public.destination_deliveries USING btree (destination_id);

--
-- Name: ix_destination_deliveries_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_deliveries_lead_id ON public.destination_deliveries USING btree (lead_id);

--
-- Name: ix_destination_deliveries_run_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_deliveries_run_id ON public.destination_deliveries USING btree (run_id);

--
-- Name: ix_destination_deliveries_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_deliveries_workspace_id ON public.destination_deliveries USING btree (workspace_id);

--
-- Name: ix_destination_deliveries_workspace_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_deliveries_workspace_run ON public.destination_deliveries USING btree (workspace_id, run_id);

--
-- Name: ix_destination_inbound_receipts_destination_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_receipts_destination_id ON public.destination_inbound_receipts USING btree (destination_id);

--
-- Name: ix_destination_inbound_receipts_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_receipts_lead_id ON public.destination_inbound_receipts USING btree (lead_id);

--
-- Name: ix_destination_inbound_receipts_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_receipts_workspace_id ON public.destination_inbound_receipts USING btree (workspace_id);

--
-- Name: ix_destination_inbound_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ix_destination_inbound_token_hash ON public.destination_inbound_tokens USING btree (token_hash);

--
-- Name: ix_destination_inbound_tokens_destination_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_tokens_destination_id ON public.destination_inbound_tokens USING btree (destination_id);

--
-- Name: ix_destination_inbound_tokens_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_tokens_workspace_id ON public.destination_inbound_tokens USING btree (workspace_id);

--
-- Name: ix_destination_inbound_ws_destination_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_ws_destination_created ON public.destination_inbound_receipts USING btree (workspace_id, destination_id, created_at);

--
-- Name: ix_destination_inbound_ws_destination_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_inbound_ws_destination_cursor ON public.destination_inbound_receipts USING btree (workspace_id, destination_id, created_at, id);

--
-- Name: ix_destination_runs_destination_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_runs_destination_id ON public.destination_runs USING btree (destination_id);

--
-- Name: ix_destination_runs_workspace_destination_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_runs_workspace_destination_created ON public.destination_runs USING btree (workspace_id, destination_id, created_at);

--
-- Name: ix_destination_runs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_runs_workspace_id ON public.destination_runs USING btree (workspace_id);

--
-- Name: ix_destination_runs_ws_destination_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_destination_runs_ws_destination_cursor ON public.destination_runs USING btree (workspace_id, destination_id, created_at, id);

--
-- Name: ix_email_data_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_email_data_id ON public.email_data USING btree (id);

--
-- Name: ix_email_verify_cache_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_email_verify_cache_expires ON public.email_verify_cache USING btree (expires_at);

--
-- Name: ix_enrichment_cache_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_enrichment_cache_expires ON public.enrichment_cache USING btree (expires_at);

--
-- Name: ix_enroll_ws_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_enroll_ws_due ON public.outreach_enrollments USING btree (workspace_id, status, next_send_at);

--
-- Name: ix_enroll_ws_seq_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_enroll_ws_seq_status ON public.outreach_enrollments USING btree (workspace_id, sequence_id, status);

--
-- Name: ix_entity_blocking_key_entity; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_blocking_key_entity ON public.entity_blocking_keys USING btree (key, entity_id);

--
-- Name: ix_entity_blocking_keys_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_blocking_keys_entity_id ON public.entity_blocking_keys USING btree (entity_id);

--
-- Name: ix_entity_blocking_keys_key; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_blocking_keys_key ON public.entity_blocking_keys USING btree (key);

--
-- Name: ix_entity_blocking_keys_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_blocking_keys_workspace_id ON public.entity_blocking_keys USING btree (workspace_id);

--
-- Name: ix_entity_merge_log_kept_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_merge_log_kept_id ON public.entity_merge_log USING btree (kept_id);

--
-- Name: ix_entity_merge_log_merged_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_merge_log_merged_id ON public.entity_merge_log USING btree (merged_id);

--
-- Name: ix_entity_merge_log_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_merge_log_workspace_id ON public.entity_merge_log USING btree (workspace_id);

--
-- Name: ix_entity_review_pairs_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_review_pairs_entity_id ON public.entity_review_pairs USING btree (entity_id);

--
-- Name: ix_entity_review_pairs_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_review_pairs_status ON public.entity_review_pairs USING btree (status);

--
-- Name: ix_entity_review_pairs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_entity_review_pairs_workspace_id ON public.entity_review_pairs USING btree (workspace_id);

--
-- Name: ix_governance_audit_events_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_governance_audit_events_workspace_id ON public.governance_audit_events USING btree (workspace_id);

--
-- Name: ix_governance_audit_request; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_governance_audit_request ON public.governance_audit_events USING btree (request_id);

--
-- Name: ix_governance_audit_ws_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_governance_audit_ws_actor ON public.governance_audit_events USING btree (workspace_id, actor_user_id);

--
-- Name: ix_governance_audit_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_governance_audit_ws_created ON public.governance_audit_events USING btree (workspace_id, created_at);

--
-- Name: ix_governance_audit_ws_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_governance_audit_ws_cursor ON public.governance_audit_events USING btree (workspace_id, created_at, id);

--
-- Name: ix_inbound_sched_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_inbound_sched_due ON public.outreach_inbound_schedules USING btree (enabled, next_poll_at);

--
-- Name: ix_inbound_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_inbound_ws_created ON public.outreach_inbound_messages USING btree (workspace_id, created_at);

--
-- Name: ix_jobs_claim; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_jobs_claim ON public.jobs USING btree (executor, status, priority DESC, next_run_at) WHERE ((status)::text = 'pending'::text);

--
-- Name: ix_jobs_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_jobs_id ON public.jobs USING btree (id);

--
-- Name: ix_jobs_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_jobs_status ON public.jobs USING btree (status);

--
-- Name: ix_jobs_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_jobs_type ON public.jobs USING btree (type);

--
-- Name: ix_jobs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_jobs_workspace_id ON public.jobs USING btree (workspace_id);

--
-- Name: ix_leads_collection_job_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_collection_job_id ON public.leads USING btree (collection_job_id);

--
-- Name: ix_leads_search_tsv; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_search_tsv ON public.leads USING gin (search_tsv);

--
-- Name: ix_leads_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_workspace_id ON public.leads USING btree (workspace_id);

--
-- Name: ix_leads_ws_collection_job; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_ws_collection_job ON public.leads USING btree (workspace_id, collection_job_id);

--
-- Name: ix_leads_ws_score; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_ws_score ON public.leads USING btree (workspace_id, score);

--
-- Name: ix_leads_ws_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_ws_source ON public.leads USING btree (workspace_id, source);

--
-- Name: ix_leads_ws_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_leads_ws_status ON public.leads USING btree (workspace_id, status);

--
-- Name: ix_ledger_workspace_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_ledger_workspace_created ON public.credit_ledger_entries USING btree (workspace_id, created_at);

--
-- Name: ix_links_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_links_id ON public.links USING btree (id);

--
-- Name: ix_links_url; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ix_links_url ON public.links USING btree (url);

--
-- Name: ix_llm_usage_daily_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_llm_usage_daily_workspace_id ON public.llm_usage_daily USING btree (workspace_id);

--
-- Name: ix_llm_usage_ws_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_llm_usage_ws_date ON public.llm_usage_daily USING btree (workspace_id, date);

--
-- Name: ix_mcp_audit_log_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_mcp_audit_log_workspace_id ON public.mcp_audit_log USING btree (workspace_id);

--
-- Name: ix_mcp_audit_log_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_mcp_audit_log_ws_created ON public.mcp_audit_log USING btree (workspace_id, created_at);

--
-- Name: ix_mcp_tokens_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ix_mcp_tokens_token_hash ON public.mcp_tokens USING btree (token_hash);

--
-- Name: ix_mcp_tokens_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_mcp_tokens_user_id ON public.mcp_tokens USING btree (user_id);

--
-- Name: ix_mcp_tokens_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_mcp_tokens_workspace_id ON public.mcp_tokens USING btree (workspace_id);

--
-- Name: ix_outreach_draft_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_draft_ws_created ON public.outreach_drafts USING btree (workspace_id, created_at);

--
-- Name: ix_outreach_draft_ws_person; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_draft_ws_person ON public.outreach_drafts USING btree (workspace_id, person_id);

--
-- Name: ix_outreach_sched_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_sched_due ON public.outreach_schedules USING btree (enabled, next_tick_at);

--
-- Name: ix_outreach_sends_message_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_sends_message_id ON public.outreach_sends USING btree (message_id);

--
-- Name: ix_outreach_seq_ws; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_seq_ws ON public.outreach_sequences USING btree (workspace_id);

--
-- Name: ix_outreach_seq_ws_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_outreach_seq_ws_status ON public.outreach_sequences USING btree (workspace_id, status);

--
-- Name: ix_person_employments_company_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_employments_company_entity_id ON public.person_employments USING btree (company_entity_id);

--
-- Name: ix_person_employments_person_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_employments_person_id ON public.person_employments USING btree (person_id);

--
-- Name: ix_person_employments_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_employments_workspace_id ON public.person_employments USING btree (workspace_id);

--
-- Name: ix_person_entities_company_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_entities_company_entity_id ON public.person_entities USING btree (company_entity_id);

--
-- Name: ix_person_entities_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_entities_workspace_id ON public.person_entities USING btree (workspace_id);

--
-- Name: ix_person_identifiers_person_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_identifiers_person_id ON public.person_identifiers USING btree (person_id);

--
-- Name: ix_person_identifiers_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_identifiers_workspace_id ON public.person_identifiers USING btree (workspace_id);

--
-- Name: ix_person_intel_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_intel_id ON public.person_intel USING btree (id);

--
-- Name: ix_person_intel_linkedin_url; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_intel_linkedin_url ON public.person_intel USING btree (linkedin_url);

--
-- Name: ix_person_intel_username; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_person_intel_username ON public.person_intel USING btree (username);

--
-- Name: ix_playbook_results_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_results_lead_id ON public.playbook_results USING btree (lead_id);

--
-- Name: ix_playbook_results_run_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_results_run_id ON public.playbook_results USING btree (run_id);

--
-- Name: ix_playbook_results_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_results_workspace_id ON public.playbook_results USING btree (workspace_id);

--
-- Name: ix_playbook_results_ws_run_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_results_ws_run_cursor ON public.playbook_results USING btree (workspace_id, run_id, id);

--
-- Name: ix_playbook_runs_audience_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_runs_audience_id ON public.playbook_runs USING btree (audience_id);

--
-- Name: ix_playbook_runs_playbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_runs_playbook_id ON public.playbook_runs USING btree (playbook_id);

--
-- Name: ix_playbook_runs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_runs_workspace_id ON public.playbook_runs USING btree (workspace_id);

--
-- Name: ix_playbook_runs_ws_playbook_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_runs_ws_playbook_created ON public.playbook_runs USING btree (workspace_id, playbook_id, created_at);

--
-- Name: ix_playbook_runs_ws_playbook_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_runs_ws_playbook_cursor ON public.playbook_runs USING btree (workspace_id, playbook_id, created_at, id);

--
-- Name: ix_playbook_schedules_due_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_schedules_due_cursor ON public.playbook_schedules USING btree (enabled, next_run_at, playbook_id);

--
-- Name: ix_playbook_schedules_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_playbook_schedules_workspace_id ON public.playbook_schedules USING btree (workspace_id);

--
-- Name: ix_provider_stats_field; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_provider_stats_field ON public.provider_stats USING btree (field);

--
-- Name: ix_provider_stats_provider; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_provider_stats_provider ON public.provider_stats USING btree (provider);

--
-- Name: ix_research_playbooks_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_research_playbooks_workspace_id ON public.research_playbooks USING btree (workspace_id);

--
-- Name: ix_retention_runs_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_retention_runs_workspace_id ON public.retention_runs USING btree (workspace_id);

--
-- Name: ix_retention_runs_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_retention_runs_ws_created ON public.retention_runs USING btree (workspace_id, created_at);

--
-- Name: ix_retention_runs_ws_cursor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_retention_runs_ws_cursor ON public.retention_runs USING btree (workspace_id, created_at, id);

--
-- Name: ix_scheduled_triggers_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_scheduled_triggers_due ON public.scheduled_triggers USING btree (enabled, next_run_at);

--
-- Name: ix_scrape_history_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_scrape_history_id ON public.scrape_history USING btree (id);

--
-- Name: ix_scrape_history_url; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_scrape_history_url ON public.scrape_history USING btree (url);

--
-- Name: ix_send_ws_msgid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_send_ws_msgid ON public.outreach_sends USING btree (workspace_id, message_id);

--
-- Name: ix_send_ws_sent_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_send_ws_sent_at ON public.outreach_sends USING btree (workspace_id, sent_at);

--
-- Name: ix_send_ws_seq_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_send_ws_seq_created ON public.outreach_sends USING btree (workspace_id, sequence_id, created_at);

--
-- Name: ix_send_ws_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_send_ws_status ON public.outreach_sends USING btree (workspace_id, status);

--
-- Name: ix_signals_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_signals_workspace_id ON public.signals USING btree (workspace_id);

--
-- Name: ix_signals_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_signals_ws_created ON public.signals USING btree (workspace_id, created_at);

--
-- Name: ix_signals_ws_created_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_signals_ws_created_id ON public.signals USING btree (workspace_id, created_at, id);

--
-- Name: ix_signals_ws_lead; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_signals_ws_lead ON public.signals USING btree (workspace_id, lead_id);

--
-- Name: ix_signals_ws_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_signals_ws_type ON public.signals USING btree (workspace_id, signal_type);

--
-- Name: ix_source_stats_region; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_source_stats_region ON public.source_stats USING btree (region);

--
-- Name: ix_source_stats_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_source_stats_source ON public.source_stats USING btree (source);

--
-- Name: ix_suppress_ws_email; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_suppress_ws_email ON public.outreach_suppressions USING btree (workspace_id, email);

--
-- Name: ix_tar_ws_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_tar_ws_created ON public.trigger_action_results USING btree (workspace_id, created_at);

--
-- Name: ix_tar_ws_trigger_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_tar_ws_trigger_created ON public.trigger_action_results USING btree (workspace_id, trigger_id, created_at);

--
-- Name: ix_trigger_runs_ws; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_trigger_runs_ws ON public.trigger_runs USING btree (workspace_id);

--
-- Name: ix_trigger_runs_ws_trigger_started; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_trigger_runs_ws_trigger_started ON public.trigger_runs USING btree (workspace_id, trigger_id, started_at);

--
-- Name: ix_triggers_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_triggers_workspace_id ON public.triggers USING btree (workspace_id);

--
-- Name: ix_triggers_ws_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_triggers_ws_enabled ON public.triggers USING btree (workspace_id, enabled);

--
-- Name: ix_triggers_ws_next_run; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_triggers_ws_next_run ON public.triggers USING btree (workspace_id, trigger_type, next_run_at);

--
-- Name: ix_triggers_ws_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_triggers_ws_type ON public.triggers USING btree (workspace_id, trigger_type);

--
-- Name: ix_users_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_users_id ON public.users USING btree (id);

--
-- Name: ix_users_username; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ix_users_username ON public.users USING btree (username);

--
-- Name: ix_watch_sched_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_watch_sched_due ON public.watch_schedules USING btree (enabled, next_poll_at);

--
-- Name: ix_watch_ws_created_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_watch_ws_created_id ON public.watch_subscriptions USING btree (workspace_id, created_at, id);

--
-- Name: ix_watch_ws_enabled; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_watch_ws_enabled ON public.watch_subscriptions USING btree (workspace_id, enabled);

--
-- Name: ix_watch_ws_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_watch_ws_kind ON public.watch_subscriptions USING btree (workspace_id, kind);

--
-- Name: ix_workbook_activity_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_activity_created_at ON public.workbook_activity USING btree (created_at);

--
-- Name: ix_workbook_activity_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_activity_workbook_id ON public.workbook_activity USING btree (workbook_id);

--
-- Name: ix_workbook_activity_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_activity_workspace_id ON public.workbook_activity USING btree (workspace_id);

--
-- Name: ix_workbook_enrichments_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_enrichments_lead_id ON public.workbook_enrichments USING btree (lead_id);

--
-- Name: ix_workbook_enrichments_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_enrichments_workbook_id ON public.workbook_enrichments USING btree (workbook_id);

--
-- Name: ix_workbook_enrichments_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_enrichments_workspace_id ON public.workbook_enrichments USING btree (workspace_id);

--
-- Name: ix_workbook_ingest_idem_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_ingest_idem_workbook_id ON public.workbook_ingest_idempotency USING btree (workbook_id);

--
-- Name: ix_workbook_ingest_idem_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_ingest_idem_workspace_id ON public.workbook_ingest_idempotency USING btree (workspace_id);

--
-- Name: ix_workbook_ingest_tokens_token_hash; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX ix_workbook_ingest_tokens_token_hash ON public.workbook_ingest_tokens USING btree (token_hash);

--
-- Name: ix_workbook_ingest_tokens_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_ingest_tokens_workbook_id ON public.workbook_ingest_tokens USING btree (workbook_id);

--
-- Name: ix_workbook_ingest_tokens_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_ingest_tokens_workspace_id ON public.workbook_ingest_tokens USING btree (workspace_id);

--
-- Name: ix_workbook_rows_canonical_entity_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_canonical_entity_id ON public.workbook_rows USING btree (canonical_entity_id);

--
-- Name: ix_workbook_rows_data_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_data_trgm ON public.workbook_rows USING gin (lower((data)::text) public.gin_trgm_ops);

--
-- Name: ix_workbook_rows_enrichments_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_enrichments_trgm ON public.workbook_rows USING gin (lower((enrichments)::text) public.gin_trgm_ops);

--
-- Name: ix_workbook_rows_lead_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_lead_id ON public.workbook_rows USING btree (lead_id);

--
-- Name: ix_workbook_rows_sort_city; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_city ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'city'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_company; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_company ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'company'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_company_size; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_company_size ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'company_size'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_contact_person; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_contact_person ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'contact_person'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_contact_title; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_contact_title ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'contact_title'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_email; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_email ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'email'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_score; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_score ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'score'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_score_tier; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_score_tier ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'score_tier'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_source ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'source'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_state; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_state ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'state'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_status ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'status'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_sort_website; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_sort_website ON public.workbook_rows USING btree (workbook_id, COALESCE((data ->> 'website'::text), ''::text), "position", id);

--
-- Name: ix_workbook_rows_source_provider; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_source_provider ON public.workbook_rows USING btree (source_provider);

--
-- Name: ix_workbook_rows_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_workbook_id ON public.workbook_rows USING btree (workbook_id);

--
-- Name: ix_workbook_rows_workbook_position_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_workbook_position_id ON public.workbook_rows USING btree (workbook_id, "position", id);

--
-- Name: ix_workbook_rows_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_rows_workspace_id ON public.workbook_rows USING btree (workspace_id);

--
-- Name: ix_workbook_spend_exposure; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_spend_exposure ON public.workbook_spend_attempts USING btree (workspace_id, workbook_id, status);

--
-- Name: ix_workbook_views_workbook_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_views_workbook_id ON public.workbook_views USING btree (workbook_id);

--
-- Name: ix_workbook_views_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbook_views_workspace_id ON public.workbook_views USING btree (workspace_id);

--
-- Name: ix_workbooks_workspace_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ix_workbooks_workspace_id ON public.workbooks USING btree (workspace_id);

--
-- Name: uq_connector_runs_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_connector_runs_active ON public.connector_runs USING btree (workbook_id, connector) WHERE ((status)::text = ANY (ARRAY[('pending'::character varying)::text, ('running'::character varying)::text, ('retrying'::character varying)::text]));

--
-- Name: uq_jobs_fire_key_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_jobs_fire_key_active ON public.jobs USING btree (fire_key) WHERE ((status)::text = ANY (ARRAY[('pending'::character varying)::text, ('processing'::character varying)::text]));

--
-- Name: leads leads_search_tsv_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER leads_search_tsv_update BEFORE INSERT OR UPDATE ON public.leads FOR EACH ROW EXECUTE FUNCTION public.leads_search_tsv_trigger();

--
-- Name: audience_destinations audience_destinations_audience_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_destinations
    ADD CONSTRAINT audience_destinations_audience_id_fkey FOREIGN KEY (audience_id) REFERENCES public.audiences(id) ON DELETE CASCADE;

--
-- Name: audience_members audience_members_audience_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_members
    ADD CONSTRAINT audience_members_audience_id_fkey FOREIGN KEY (audience_id) REFERENCES public.audiences(id) ON DELETE CASCADE;

--
-- Name: audience_membership_events audience_membership_events_audience_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audience_membership_events
    ADD CONSTRAINT audience_membership_events_audience_id_fkey FOREIGN KEY (audience_id) REFERENCES public.audiences(id) ON DELETE CASCADE;

--
-- Name: chat_messages chat_messages_conversation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.chat_messages
    ADD CONSTRAINT chat_messages_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES public.chat_conversations(id) ON DELETE CASCADE;

--
-- Name: collection_job_stages collection_job_stages_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.collection_job_stages
    ADD CONSTRAINT collection_job_stages_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.collection_jobs(id) ON DELETE CASCADE;

--
-- Name: company_identifiers company_identifiers_entity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.company_identifiers
    ADD CONSTRAINT company_identifiers_entity_id_fkey FOREIGN KEY (entity_id) REFERENCES public.company_entities(id) ON DELETE CASCADE;

--
-- Name: connector_runs connector_runs_workbook_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.connector_runs
    ADD CONSTRAINT connector_runs_workbook_id_fkey FOREIGN KEY (workbook_id) REFERENCES public.workbooks(id) ON DELETE CASCADE;

--
-- Name: destination_deliveries destination_deliveries_destination_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_deliveries
    ADD CONSTRAINT destination_deliveries_destination_id_fkey FOREIGN KEY (destination_id) REFERENCES public.audience_destinations(id) ON DELETE CASCADE;

--
-- Name: destination_deliveries destination_deliveries_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_deliveries
    ADD CONSTRAINT destination_deliveries_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.destination_runs(id) ON DELETE CASCADE;

--
-- Name: destination_runs destination_runs_destination_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.destination_runs
    ADD CONSTRAINT destination_runs_destination_id_fkey FOREIGN KEY (destination_id) REFERENCES public.audience_destinations(id) ON DELETE CASCADE;

--
-- Name: entity_blocking_keys entity_blocking_keys_entity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.entity_blocking_keys
    ADD CONSTRAINT entity_blocking_keys_entity_id_fkey FOREIGN KEY (entity_id) REFERENCES public.company_entities(id) ON DELETE CASCADE;

--
-- Name: research_playbooks fk_research_playbooks_schedule_audience; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.research_playbooks
    ADD CONSTRAINT fk_research_playbooks_schedule_audience FOREIGN KEY (schedule_audience_id) REFERENCES public.audiences(id) ON DELETE SET NULL;

--
-- Name: person_employments person_employments_company_entity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_employments
    ADD CONSTRAINT person_employments_company_entity_id_fkey FOREIGN KEY (company_entity_id) REFERENCES public.company_entities(id) ON DELETE SET NULL;

--
-- Name: person_employments person_employments_person_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_employments
    ADD CONSTRAINT person_employments_person_id_fkey FOREIGN KEY (person_id) REFERENCES public.person_entities(id) ON DELETE CASCADE;

--
-- Name: person_entities person_entities_company_entity_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_entities
    ADD CONSTRAINT person_entities_company_entity_id_fkey FOREIGN KEY (company_entity_id) REFERENCES public.company_entities(id);

--
-- Name: person_identifiers person_identifiers_person_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.person_identifiers
    ADD CONSTRAINT person_identifiers_person_id_fkey FOREIGN KEY (person_id) REFERENCES public.person_entities(id) ON DELETE CASCADE;

--
-- Name: playbook_results playbook_results_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_results
    ADD CONSTRAINT playbook_results_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.playbook_runs(id) ON DELETE CASCADE;

--
-- Name: playbook_runs playbook_runs_audience_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_runs
    ADD CONSTRAINT playbook_runs_audience_id_fkey FOREIGN KEY (audience_id) REFERENCES public.audiences(id) ON DELETE CASCADE;

--
-- Name: playbook_runs playbook_runs_playbook_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.playbook_runs
    ADD CONSTRAINT playbook_runs_playbook_id_fkey FOREIGN KEY (playbook_id) REFERENCES public.research_playbooks(id) ON DELETE CASCADE;

--
-- Name: workbook_enrichments workbook_enrichments_workbook_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_enrichments
    ADD CONSTRAINT workbook_enrichments_workbook_id_fkey FOREIGN KEY (workbook_id) REFERENCES public.workbooks(id) ON DELETE CASCADE;

--
-- Name: workbook_rows workbook_rows_workbook_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_rows
    ADD CONSTRAINT workbook_rows_workbook_id_fkey FOREIGN KEY (workbook_id) REFERENCES public.workbooks(id) ON DELETE CASCADE;

--
-- Name: workbook_views workbook_views_workbook_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workbook_views
    ADD CONSTRAINT workbook_views_workbook_id_fkey FOREIGN KEY (workbook_id) REFERENCES public.workbooks(id) ON DELETE CASCADE;

--
-- Name: workspace_member_permissions workspace_member_permissions_workspace_id_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_member_permissions
    ADD CONSTRAINT workspace_member_permissions_workspace_id_user_id_fkey FOREIGN KEY (workspace_id, user_id) REFERENCES public.workspace_members(workspace_id, user_id);

--
-- Name: workspace_members workspace_members_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_members
    ADD CONSTRAINT workspace_members_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

--
-- Name: workspace_oidc_identities workspace_oidc_identities_workspace_id_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_oidc_identities
    ADD CONSTRAINT workspace_oidc_identities_workspace_id_user_id_fkey FOREIGN KEY (workspace_id, user_id) REFERENCES public.workspace_members(workspace_id, user_id);

--
-- Name: workspace_scim_group_members workspace_scim_group_members_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_group_members
    ADD CONSTRAINT workspace_scim_group_members_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.workspace_scim_groups(id) ON DELETE CASCADE;

--
-- Name: workspace_scim_groups workspace_scim_groups_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_groups
    ADD CONSTRAINT workspace_scim_groups_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

--
-- Name: workspace_scim_tokens workspace_scim_tokens_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_tokens
    ADD CONSTRAINT workspace_scim_tokens_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

--
-- Name: workspace_scim_users workspace_scim_users_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_scim_users
    ADD CONSTRAINT workspace_scim_users_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

--
-- Name: workspace_settings workspace_settings_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_settings
    ADD CONSTRAINT workspace_settings_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id);

--
-- Name: audience_destinations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audience_destinations ENABLE ROW LEVEL SECURITY;

--
-- Name: audience_members; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audience_members ENABLE ROW LEVEL SECURITY;

--
-- Name: audience_membership_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audience_membership_events ENABLE ROW LEVEL SECURITY;

--
-- Name: audiences; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audiences ENABLE ROW LEVEL SECURITY;

--
-- Name: cell_traces; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.cell_traces ENABLE ROW LEVEL SECURITY;

--
-- Name: chat_conversations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.chat_conversations ENABLE ROW LEVEL SECURITY;

--
-- Name: chat_messages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.chat_messages ENABLE ROW LEVEL SECURITY;

--
-- Name: chat_tool_approvals; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.chat_tool_approvals ENABLE ROW LEVEL SECURITY;

--
-- Name: collection_job_stages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.collection_job_stages ENABLE ROW LEVEL SECURITY;

--
-- Name: collection_jobs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.collection_jobs ENABLE ROW LEVEL SECURITY;

--
-- Name: company_entities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.company_entities ENABLE ROW LEVEL SECURITY;

--
-- Name: company_identifiers; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.company_identifiers ENABLE ROW LEVEL SECURITY;

--
-- Name: connector_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.connector_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: credit_ledger_entries; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.credit_ledger_entries ENABLE ROW LEVEL SECURITY;

--
-- Name: destination_deliveries; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.destination_deliveries ENABLE ROW LEVEL SECURITY;

--
-- Name: destination_inbound_receipts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.destination_inbound_receipts ENABLE ROW LEVEL SECURITY;

--
-- Name: destination_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.destination_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: email_verify_cache; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.email_verify_cache ENABLE ROW LEVEL SECURITY;

--
-- Name: enrichment_cache; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.enrichment_cache ENABLE ROW LEVEL SECURITY;

--
-- Name: entity_blocking_keys; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.entity_blocking_keys ENABLE ROW LEVEL SECURITY;

--
-- Name: entity_merge_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.entity_merge_log ENABLE ROW LEVEL SECURITY;

--
-- Name: entity_review_pairs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.entity_review_pairs ENABLE ROW LEVEL SECURITY;

--
-- Name: governance_audit_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.governance_audit_events ENABLE ROW LEVEL SECURITY;

--
-- Name: leads; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY;

--
-- Name: llm_usage_daily; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.llm_usage_daily ENABLE ROW LEVEL SECURITY;

--
-- Name: mcp_audit_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.mcp_audit_log ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_drafts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_drafts ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_drafts outreach_drafts_workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY outreach_drafts_workspace_isolation ON public.outreach_drafts USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_enrollments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_enrollments ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_inbound_messages; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_inbound_messages ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_sends; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_sends ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_sequences; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_sequences ENABLE ROW LEVEL SECURITY;

--
-- Name: outreach_suppressions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.outreach_suppressions ENABLE ROW LEVEL SECURITY;

--
-- Name: person_employments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.person_employments ENABLE ROW LEVEL SECURITY;

--
-- Name: person_entities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.person_entities ENABLE ROW LEVEL SECURITY;

--
-- Name: person_identifiers; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.person_identifiers ENABLE ROW LEVEL SECURITY;

--
-- Name: playbook_results; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.playbook_results ENABLE ROW LEVEL SECURITY;

--
-- Name: playbook_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.playbook_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: poll_budget_ledger; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.poll_budget_ledger ENABLE ROW LEVEL SECURITY;

--
-- Name: research_playbooks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.research_playbooks ENABLE ROW LEVEL SECURITY;

--
-- Name: retention_policies; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.retention_policies ENABLE ROW LEVEL SECURITY;

--
-- Name: retention_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.retention_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: signals; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.signals ENABLE ROW LEVEL SECURITY;

--
-- Name: trigger_action_results; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.trigger_action_results ENABLE ROW LEVEL SECURITY;

--
-- Name: trigger_cap_reservations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.trigger_cap_reservations ENABLE ROW LEVEL SECURITY;

--
-- Name: trigger_runs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.trigger_runs ENABLE ROW LEVEL SECURITY;

--
-- Name: triggers; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.triggers ENABLE ROW LEVEL SECURITY;

--
-- Name: watch_subscriptions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.watch_subscriptions ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_activity; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_activity ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_enrichments; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_enrichments ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_ingest_idempotency; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_ingest_idempotency ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_rows; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_rows ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_spend_attempts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_spend_attempts ENABLE ROW LEVEL SECURITY;

--
-- Name: workbook_spend_attempts workbook_spend_attempts_workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workbook_spend_attempts_workspace_isolation ON public.workbook_spend_attempts USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_views; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbook_views ENABLE ROW LEVEL SECURITY;

--
-- Name: workbooks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workbooks ENABLE ROW LEVEL SECURITY;

--
-- Name: workspace_credits; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_credits ENABLE ROW LEVEL SECURITY;

--
-- Name: audience_destinations workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.audience_destinations USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: audience_members workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.audience_members USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: audience_membership_events workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.audience_membership_events USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: audiences workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.audiences USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: cell_traces workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.cell_traces USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: chat_conversations workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.chat_conversations USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: chat_messages workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.chat_messages USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: chat_tool_approvals workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.chat_tool_approvals USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: collection_job_stages workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.collection_job_stages USING ((workspace_id = current_setting('app.workspace_id'::text, true))) WITH CHECK ((workspace_id = current_setting('app.workspace_id'::text, true)));

--
-- Name: collection_jobs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.collection_jobs USING ((workspace_id = current_setting('app.workspace_id'::text, true))) WITH CHECK ((workspace_id = current_setting('app.workspace_id'::text, true)));

--
-- Name: company_entities workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.company_entities USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: company_identifiers workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.company_identifiers USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: connector_runs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.connector_runs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: credit_ledger_entries workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.credit_ledger_entries USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: destination_deliveries workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.destination_deliveries USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: destination_inbound_receipts workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.destination_inbound_receipts USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: destination_runs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.destination_runs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: email_verify_cache workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.email_verify_cache USING ((workspace_id = current_setting('app.workspace_id'::text, true))) WITH CHECK ((workspace_id = current_setting('app.workspace_id'::text, true)));

--
-- Name: enrichment_cache workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.enrichment_cache USING ((workspace_id = current_setting('app.workspace_id'::text, true))) WITH CHECK ((workspace_id = current_setting('app.workspace_id'::text, true)));

--
-- Name: entity_blocking_keys workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.entity_blocking_keys USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: entity_merge_log workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.entity_merge_log USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: entity_review_pairs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.entity_review_pairs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: governance_audit_events workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.governance_audit_events USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: leads workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.leads USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: llm_usage_daily workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.llm_usage_daily USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: mcp_audit_log workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.mcp_audit_log USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_enrollments workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.outreach_enrollments USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_inbound_messages workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.outreach_inbound_messages USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_sends workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.outreach_sends USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_sequences workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.outreach_sequences USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: outreach_suppressions workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.outreach_suppressions USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: person_employments workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.person_employments USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: person_entities workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.person_entities USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: person_identifiers workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.person_identifiers USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: playbook_results workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.playbook_results USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: playbook_runs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.playbook_runs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: poll_budget_ledger workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.poll_budget_ledger USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: research_playbooks workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.research_playbooks USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: retention_policies workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.retention_policies USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: retention_runs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.retention_runs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: signals workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.signals USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: trigger_action_results workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.trigger_action_results USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: trigger_cap_reservations workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.trigger_cap_reservations USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: trigger_runs workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.trigger_runs USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: triggers workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.triggers USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: watch_subscriptions workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.watch_subscriptions USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_activity workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbook_activity USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_enrichments workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbook_enrichments USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_ingest_idempotency workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbook_ingest_idempotency USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_rows workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbook_rows USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbook_views workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbook_views USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workbooks workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workbooks USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_credits workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_credits USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_member_permissions workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_member_permissions USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_scim_group_members workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_scim_group_members USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_scim_groups workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_scim_groups USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_scim_users workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_scim_users USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_settings workspace_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY workspace_isolation ON public.workspace_settings USING (((workspace_id)::text = current_setting('app.workspace_id'::text, true))) WITH CHECK (((workspace_id)::text = current_setting('app.workspace_id'::text, true)));

--
-- Name: workspace_member_permissions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_member_permissions ENABLE ROW LEVEL SECURITY;

--
-- Name: workspace_scim_group_members; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_scim_group_members ENABLE ROW LEVEL SECURITY;

--
-- Name: workspace_scim_groups; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_scim_groups ENABLE ROW LEVEL SECURITY;

--
-- Name: workspace_scim_users; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_scim_users ENABLE ROW LEVEL SECURITY;

--
-- Name: workspace_settings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.workspace_settings ENABLE ROW LEVEL SECURITY;

--
-- PostgreSQL database dump complete
--

