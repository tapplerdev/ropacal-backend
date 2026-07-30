-- Baseline: the complete schema, as it exists in production.
--
-- WHY THIS IS A pg_dump AND NOT HAND-WRITTEN. The schema had drifted into two
-- places that disagreed: an idempotent DDL list that re-ran on every boot
-- (internal/database/database.go) and migrations/*.sql files applied by hand with
-- nothing recording that they had run. Production ended up correct by accretion;
-- the REPO could not rebuild it. Verified 2026-07-30 by pointing the server at an
-- empty database — it aborted at boot, and 24 of the 223 boot statements failed
-- across 4 tables. route_tasks, the central table of the itinerary domain, had no
-- CREATE TABLE anywhere in the repository at all (internal/itinerary/DESIGN.md
-- line 76 lists exactly that as unshipped Phase 0 work); routes and route_bins
-- were missing too.
--
-- Hand-authoring the missing DDL would have meant guessing at production's real
-- shape. Dumping it guarantees a byte-accurate match, in correct dependency
-- order, including the RLS policies and the composite (organization_id, id)
-- unique constraints the tenancy migration added.
--
-- PRODUCTION IS BASELINED, NOT MIGRATED. It already has every object here, so it
-- is marked as having applied this version without executing it (see
-- database.BaselineIfNeeded). Running it there would fail on the first CREATE.
-- Only a genuinely fresh database executes this file.
--
-- NOT included, because pg_dump --schema-only cannot carry them:
--   * the binly_app role (cluster-level, not per-database) — a fresh environment
--     must create it and grant ownership; see TENANCY_BACKLOG.md
--   * any data, including seed users/bins
--
-- Generated from production 2026-07-30 with:
--   pg_dump --schema-only --no-owner --no-privileges --no-comments

-- +goose Up

--
-- PostgreSQL database dump
--


-- Dumped from database version 17.7 (Debian 17.7-3.pgdg13+1)
-- Dumped by pg_dump version 17.9 (Homebrew)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
-- pg_dump emits `set_config('search_path','',false)` here so that every object
-- in its output must be schema-qualified. That is correct for psql, but it
-- breaks goose: goose inserts its version row AFTER running this file, using an
-- unqualified `goose_db_version`, which an empty search_path cannot resolve
-- ("relation goose_db_version does not exist"). Every statement below is already
-- fully qualified as public.*, so keeping the default search_path changes
-- nothing about what gets created.
SELECT pg_catalog.set_config('search_path', 'public', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: update_route_task_timestamp(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.update_route_task_timestamp() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
  BEGIN
    NEW.updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT;
    RETURN NEW;
  END;
  $$;
-- +goose StatementEnd


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: ai_recommendations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ai_recommendations (
    id text NOT NULL,
    type text NOT NULL,
    entity_type text NOT NULL,
    entity_id text,
    title text NOT NULL,
    description text NOT NULL,
    severity text DEFAULT 'medium'::text,
    recommended_action text,
    status text DEFAULT 'pending'::text,
    source text DEFAULT 'ai_agent'::text,
    reasoning text,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    expires_at bigint,
    actioned_at bigint,
    actioned_by_user_id text,
    snoozed_until bigint,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.ai_recommendations FORCE ROW LEVEL SECURITY;


--
-- Name: airtag_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.airtag_accounts (
    id text NOT NULL,
    email text NOT NULL,
    password text NOT NULL,
    account_state text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.airtag_accounts FORCE ROW LEVEL SECURITY;


--
-- Name: airtag_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.airtag_keys (
    id text NOT NULL,
    account_id text NOT NULL,
    name text NOT NULL,
    tag_uuid text NOT NULL,
    private_key text NOT NULL,
    shared_secret text NOT NULL,
    secondary_shared_secret text NOT NULL,
    pairing_date text,
    product_id bigint,
    created_at bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.airtag_keys FORCE ROW LEVEL SECURITY;


--
-- Name: airtag_locations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.airtag_locations (
    id text NOT NULL,
    bin_number integer,
    name text NOT NULL,
    latitude double precision NOT NULL,
    longitude double precision NOT NULL,
    address text DEFAULT ''::text NOT NULL,
    city text DEFAULT ''::text NOT NULL,
    last_seen timestamp with time zone NOT NULL,
    battery_status integer DEFAULT 0 NOT NULL,
    is_matched boolean DEFAULT true NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.airtag_locations FORCE ROW LEVEL SECURITY;


--
-- Name: app_error_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_error_logs (
    id text NOT NULL,
    driver_id text,
    shift_id text,
    task_id text,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    log_timestamp bigint NOT NULL,
    context text NOT NULL,
    error_type text NOT NULL,
    error_message text NOT NULL,
    severity text NOT NULL,
    platform text NOT NULL,
    app_version text,
    os_version text,
    device_info text,
    last_gps_latitude double precision,
    last_gps_longitude double precision,
    stack_trace text,
    metadata jsonb,
    is_resolved boolean DEFAULT false,
    resolved_at bigint,
    resolved_by_user_id text,
    notes text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT app_error_logs_platform_check CHECK ((platform = ANY (ARRAY['ios'::text, 'android'::text]))),
    CONSTRAINT app_error_logs_severity_check CHECK ((severity = ANY (ARRAY['critical'::text, 'error'::text, 'warning'::text, 'info'::text])))
);

ALTER TABLE ONLY public.app_error_logs FORCE ROW LEVEL SECURITY;


--
-- Name: bin_change_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bin_change_log (
    id text NOT NULL,
    bin_id text NOT NULL,
    changed_by_user_id text NOT NULL,
    created_at bigint NOT NULL,
    change_type text NOT NULL,
    old_values jsonb NOT NULL,
    new_values jsonb NOT NULL,
    reason_category text,
    reason_notes text,
    no_go_zone_created boolean DEFAULT false NOT NULL,
    no_go_zone_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT bin_change_log_change_type_check CHECK ((change_type = ANY (ARRAY['address_change'::text, 'status_change'::text, 'fill_override'::text, 'bin_number_change'::text, 'coordinates_change'::text]))),
    CONSTRAINT bin_change_log_reason_category_check CHECK (((reason_category IS NULL) OR (reason_category = ANY (ARRAY['landlord_complaint'::text, 'theft'::text, 'vandalism'::text, 'missing'::text, 'relocation_request'::text, 'pulled_from_service'::text, 'other'::text]))))
);

ALTER TABLE ONLY public.bin_change_log FORCE ROW LEVEL SECURITY;


--
-- Name: bin_check_recommendations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bin_check_recommendations (
    id text NOT NULL,
    bin_id text NOT NULL,
    reason text DEFAULT 'time_based'::text NOT NULL,
    flagged_at bigint NOT NULL,
    days_since_check integer NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    resolved_at bigint,
    resolved_by_user_id text,
    notes text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT bin_check_recommendations_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'resolved'::text, 'dismissed'::text])))
);

ALTER TABLE ONLY public.bin_check_recommendations FORCE ROW LEVEL SECURITY;


--
-- Name: bin_features; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bin_features (
    bin_id text NOT NULL,
    features jsonb NOT NULL,
    fill_rate double precision,
    city text,
    backfilled_at bigint,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.bin_features FORCE ROW LEVEL SECURITY;


--
-- Name: bin_move_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bin_move_requests (
    id text NOT NULL,
    bin_id text NOT NULL,
    scheduled_date bigint NOT NULL,
    urgency text NOT NULL,
    requested_by text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    original_latitude double precision NOT NULL,
    original_longitude double precision NOT NULL,
    original_address text NOT NULL,
    new_latitude double precision,
    new_longitude double precision,
    new_address text,
    move_type text NOT NULL,
    disposal_action text,
    reason text,
    notes text,
    assigned_shift_id text,
    completed_at bigint,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    assignment_type character varying(20) DEFAULT 'shift'::character varying,
    assigned_user_id text,
    no_go_zone_id text,
    reason_category text,
    create_no_go_zone boolean DEFAULT false NOT NULL,
    source_potential_location_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT bin_move_requests_assignment_type_check CHECK (((assignment_type)::text = ANY ((ARRAY['shift'::character varying, 'manual'::character varying])::text[]))),
    CONSTRAINT bin_move_requests_disposal_action_check CHECK ((disposal_action = ANY (ARRAY['retire'::text, 'store'::text]))),
    CONSTRAINT bin_move_requests_move_type_check CHECK ((move_type = ANY (ARRAY['store'::text, 'pickup_only'::text, 'relocation'::text, 'redeployment'::text]))),
    CONSTRAINT bin_move_requests_reason_category_check CHECK ((reason_category = ANY (ARRAY['landlord_complaint'::text, 'theft'::text, 'vandalism'::text, 'missing'::text, 'relocation_request'::text, 'wrong_address'::text, 'administrative_correction'::text, 'warehouse_storage'::text, 'other'::text]))),
    CONSTRAINT bin_move_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'assigned'::text, 'in_progress'::text, 'completed'::text, 'cancelled'::text]))),
    CONSTRAINT bin_move_requests_urgency_check CHECK ((urgency = ANY (ARRAY['urgent'::text, 'soon'::text, 'scheduled'::text, 'overdue'::text, 'resolved'::text])))
);

ALTER TABLE ONLY public.bin_move_requests FORCE ROW LEVEL SECURITY;


--
-- Name: bin_watchlist; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bin_watchlist (
    id text NOT NULL,
    bin_id text NOT NULL,
    reason text NOT NULL,
    baseline_fill_rate double precision,
    started_at bigint NOT NULL,
    evaluate_at bigint NOT NULL,
    status text DEFAULT 'watching'::text NOT NULL,
    resolved_at bigint,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT bin_watchlist_status_check CHECK ((status = ANY (ARRAY['watching'::text, 'improved'::text, 'escalated'::text, 'dismissed'::text])))
);

ALTER TABLE ONLY public.bin_watchlist FORCE ROW LEVEL SECURITY;


--
-- Name: bins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.bins (
    id text NOT NULL,
    bin_number integer NOT NULL,
    current_street text NOT NULL,
    city text NOT NULL,
    zip text NOT NULL,
    last_moved bigint,
    last_checked bigint,
    status text NOT NULL,
    fill_percentage integer DEFAULT 0 NOT NULL,
    checked integer DEFAULT 0 NOT NULL,
    move_requested integer DEFAULT 0 NOT NULL,
    latitude double precision,
    longitude double precision,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    last_checked_at bigint,
    created_by_user_id text,
    retired_at bigint,
    retired_by_user_id text,
    placement_photo_url text,
    source_potential_location_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT bins_status_check CHECK ((status = ANY (ARRAY['active'::text, 'missing'::text, 'retired'::text, 'in_storage'::text, 'pending_move'::text])))
);

ALTER TABLE ONLY public.bins FORCE ROW LEVEL SECURITY;


--
-- Name: census_income_cache; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.census_income_cache (
    zip text NOT NULL,
    median_household_income integer,
    population integer,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL
);


--
-- Name: checks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.checks (
    id integer NOT NULL,
    bin_id text NOT NULL,
    checked_from text NOT NULL,
    fill_percentage integer,
    checked_on bigint NOT NULL,
    checked_by text,
    photo_url text,
    move_request_id text,
    shift_id text,
    bin_address_snapshot text,
    bin_latitude_snapshot double precision,
    bin_longitude_snapshot double precision,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.checks FORCE ROW LEVEL SECURITY;


--
-- Name: checks_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.checks_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: checks_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.checks_id_seq OWNED BY public.checks.id;


--
-- Name: config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.config (
    id integer NOT NULL,
    key character varying(255) NOT NULL,
    value jsonb NOT NULL,
    updated_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    updated_by character varying(255),
    created_at timestamp without time zone DEFAULT CURRENT_TIMESTAMP,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.config FORCE ROW LEVEL SECURITY;


--
-- Name: config_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.config_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: config_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.config_id_seq OWNED BY public.config.id;


--
-- Name: driver_current_location; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.driver_current_location (
    driver_id text NOT NULL,
    latitude double precision NOT NULL,
    longitude double precision NOT NULL,
    heading double precision,
    speed double precision,
    accuracy double precision,
    shift_id text,
    "timestamp" bigint NOT NULL,
    is_connected boolean DEFAULT true NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.driver_current_location FORCE ROW LEVEL SECURITY;


--
-- Name: driver_location_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.driver_location_history (
    id bigint NOT NULL,
    driver_id uuid NOT NULL,
    latitude double precision NOT NULL,
    longitude double precision NOT NULL,
    heading double precision,
    speed double precision,
    accuracy double precision,
    shift_id uuid,
    recorded_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.driver_location_history FORCE ROW LEVEL SECURITY;


--
-- Name: driver_location_history_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.driver_location_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: driver_location_history_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.driver_location_history_id_seq OWNED BY public.driver_location_history.id;


--
-- Name: driver_location_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.driver_location_snapshots (
    id integer NOT NULL,
    driver_id text NOT NULL,
    shift_id text,
    latitude double precision NOT NULL,
    longitude double precision NOT NULL,
    recorded_at bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.driver_location_snapshots FORCE ROW LEVEL SECURITY;


--
-- Name: driver_location_snapshots_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.driver_location_snapshots_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: driver_location_snapshots_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.driver_location_snapshots_id_seq OWNED BY public.driver_location_snapshots.id;


--
-- Name: driver_locations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.driver_locations (
    id integer NOT NULL,
    driver_id text NOT NULL,
    latitude double precision NOT NULL,
    longitude double precision NOT NULL,
    heading double precision,
    speed double precision,
    accuracy double precision,
    shift_id text,
    "timestamp" bigint NOT NULL,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.driver_locations FORCE ROW LEVEL SECURITY;


--
-- Name: driver_locations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.driver_locations_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: driver_locations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.driver_locations_id_seq OWNED BY public.driver_locations.id;


--
-- Name: fcm_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.fcm_tokens (
    id integer NOT NULL,
    user_id text NOT NULL,
    token text NOT NULL,
    device_type text NOT NULL,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT fcm_tokens_device_type_check CHECK ((device_type = ANY (ARRAY['ios'::text, 'android'::text])))
);

ALTER TABLE ONLY public.fcm_tokens FORCE ROW LEVEL SECURITY;


--
-- Name: fcm_tokens_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.fcm_tokens_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: fcm_tokens_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.fcm_tokens_id_seq OWNED BY public.fcm_tokens.id;


--
-- Name: move_request_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.move_request_history (
    id text DEFAULT (gen_random_uuid())::text NOT NULL,
    move_request_id text NOT NULL,
    action_type character varying(20) NOT NULL,
    actor_id text NOT NULL,
    actor_name text NOT NULL,
    actor_role character varying(20),
    previous_status character varying(20),
    new_status character varying(20),
    previous_assignment_type character varying(20),
    new_assignment_type character varying(20),
    previous_assigned_user_id text,
    new_assigned_user_id text,
    previous_assigned_user_name text,
    new_assigned_user_name text,
    previous_assigned_shift_id text,
    new_assigned_shift_id text,
    notes text,
    metadata jsonb,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    seq bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT move_request_history_action_type_check CHECK (((action_type)::text = ANY ((ARRAY['created'::character varying, 'assigned'::character varying, 'reassigned'::character varying, 'unassigned'::character varying, 'completed'::character varying, 'cancelled'::character varying, 'updated'::character varying])::text[])))
);

ALTER TABLE ONLY public.move_request_history FORCE ROW LEVEL SECURITY;


--
-- Name: move_request_history_seq_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.move_request_history_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: move_request_history_seq_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.move_request_history_seq_seq OWNED BY public.move_request_history.seq;


--
-- Name: moves; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.moves (
    id integer NOT NULL,
    bin_id text NOT NULL,
    moved_from text NOT NULL,
    moved_to text NOT NULL,
    moved_on bigint NOT NULL,
    move_type character varying(20) DEFAULT 'shift'::character varying,
    from_street text,
    from_city text,
    from_zip text,
    to_street text,
    to_city text,
    to_zip text,
    move_request_id text,
    completed_by_user_id text,
    shift_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT moves_move_type_check CHECK (((move_type)::text = ANY ((ARRAY['shift'::character varying, 'manual'::character varying])::text[])))
);

ALTER TABLE ONLY public.moves FORCE ROW LEVEL SECURITY;


--
-- Name: moves_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.moves_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: moves_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.moves_id_seq OWNED BY public.moves.id;


--
-- Name: no_go_zones; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.no_go_zones (
    id text NOT NULL,
    name text NOT NULL,
    center_latitude double precision NOT NULL,
    center_longitude double precision NOT NULL,
    radius_meters integer DEFAULT 500 NOT NULL,
    conflict_score integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    created_by_user_id text,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    resolved_by_user_id text,
    resolved_at bigint,
    resolution_notes text,
    merged_into_zone_id text,
    resolution_type text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT no_go_zones_resolution_type_check CHECK ((resolution_type = ANY (ARRAY['merged'::text, 'manual_resolution'::text]))),
    CONSTRAINT no_go_zones_status_check CHECK ((status = ANY (ARRAY['active'::text, 'monitoring'::text, 'resolved'::text])))
);

ALTER TABLE ONLY public.no_go_zones FORCE ROW LEVEL SECURITY;


--
-- Name: notification_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.notification_log (
    id text NOT NULL,
    type text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    data jsonb,
    recipients_count integer DEFAULT 0,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.notification_log FORCE ROW LEVEL SECURITY;


--
-- Name: organizations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.organizations (
    id text NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    CONSTRAINT organizations_status_check CHECK ((status = ANY (ARRAY['active'::text, 'suspended'::text, 'cancelled'::text])))
);

ALTER TABLE ONLY public.organizations FORCE ROW LEVEL SECURITY;


--
-- Name: placement_decisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.placement_decisions (
    id text NOT NULL,
    decided_at bigint NOT NULL,
    area_label text,
    request_seed bigint,
    candidate_lat double precision NOT NULL,
    candidate_lng double precision NOT NULL,
    features jsonb NOT NULL,
    model_version integer,
    score_at_decision double precision,
    propensity double precision,
    outcome text NOT NULL,
    reject_reason text,
    bin_id text,
    realized_fill_rate double precision,
    reward_observed_at bigint,
    partial_fill_3d double precision,
    partial_fill_7d double precision,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.placement_decisions FORCE ROW LEVEL SECURITY;


--
-- Name: potential_locations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.potential_locations (
    id text NOT NULL,
    address text NOT NULL,
    street text NOT NULL,
    city text NOT NULL,
    zip text NOT NULL,
    latitude double precision,
    longitude double precision,
    requested_by_user_id text NOT NULL,
    requested_by_name text NOT NULL,
    notes text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    converted_to_bin_id text,
    converted_at bigint,
    converted_by_user_id text,
    converted_via_shift_id text,
    assigned_shift_id text,
    converted_bin_number_snapshot integer,
    converted_address_snapshot text,
    conversion_status text,
    bin_current_status text,
    conversion_metadata jsonb,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT potential_locations_conversion_status_check CHECK ((conversion_status = ANY (ARRAY['active'::text, 'pending'::text, 'converted'::text])))
);

ALTER TABLE ONLY public.potential_locations FORCE ROW LEVEL SECURITY;


--
-- Name: route_bins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.route_bins (
    id integer NOT NULL,
    route_id text NOT NULL,
    bin_id text NOT NULL,
    sequence_order integer NOT NULL,
    created_at bigint NOT NULL,
    updated_fill_percentage integer,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.route_bins FORCE ROW LEVEL SECURITY;


--
-- Name: route_bins_id_seq1; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.route_bins_id_seq1
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: route_bins_id_seq1; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.route_bins_id_seq1 OWNED BY public.route_bins.id;


--
-- Name: route_tasks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.route_tasks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    shift_id text NOT NULL,
    sequence_order integer NOT NULL,
    task_type character varying(20) NOT NULL,
    latitude numeric(10,8) NOT NULL,
    longitude numeric(11,8) NOT NULL,
    address character varying(500),
    bin_id text,
    bin_number integer,
    fill_percentage integer,
    potential_location_id text,
    new_bin_number integer,
    move_request_id text,
    destination_latitude numeric(10,8),
    destination_longitude numeric(11,8),
    destination_address character varying(500),
    move_type character varying(50),
    warehouse_action character varying(20),
    bins_to_load integer,
    route_id character varying(100),
    is_completed integer DEFAULT 0,
    completed_at bigint,
    skipped boolean DEFAULT false,
    updated_fill_percentage integer,
    task_data jsonb,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint,
    placement_source text DEFAULT 'potential_location'::text,
    is_deleted boolean DEFAULT false,
    deleted_at bigint,
    deleted_by character varying(255),
    deletion_reason text,
    added_by text,
    addition_reason text,
    earliest_arrival timestamp with time zone,
    latest_arrival timestamp with time zone,
    time_window_type text DEFAULT 'soft'::text,
    service_duration_seconds integer DEFAULT 300,
    task_label text,
    task_description text,
    photo_required boolean DEFAULT false,
    completion_notes text,
    photo_url text,
    after_photo_url text,
    photo_latitude double precision,
    photo_longitude double precision,
    after_photo_latitude double precision,
    after_photo_longitude double precision,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT route_tasks_task_type_check CHECK (((task_type)::text = ANY ((ARRAY['collection'::character varying, 'placement'::character varying, 'pickup'::character varying, 'dropoff'::character varying, 'warehouse_stop'::character varying, 'service'::character varying])::text[]))),
    CONSTRAINT route_tasks_time_window_type_check CHECK ((time_window_type = ANY (ARRAY['strict'::text, 'soft'::text, 'soft_start'::text, 'soft_end'::text]))),
    CONSTRAINT valid_fill_percentage CHECK (((fill_percentage IS NULL) OR ((fill_percentage >= 0) AND (fill_percentage <= 100)))),
    CONSTRAINT valid_sequence CHECK ((sequence_order > 0))
);

ALTER TABLE ONLY public.route_tasks FORCE ROW LEVEL SECURITY;


--
-- Name: routes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.routes (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    geographic_area text NOT NULL,
    schedule_pattern text,
    bin_count integer DEFAULT 0,
    estimated_duration_hours numeric(4,2) DEFAULT 0,
    created_by_user_id text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.routes FORCE ROW LEVEL SECURITY;


--
-- Name: shift_bins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shift_bins (
    id integer NOT NULL,
    shift_id text NOT NULL,
    bin_id text NOT NULL,
    sequence_order integer NOT NULL,
    is_completed integer DEFAULT 0 NOT NULL,
    completed_at bigint,
    updated_fill_percentage integer,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    stop_type text DEFAULT 'collection'::text,
    move_request_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT shift_bins_stop_type_check CHECK ((stop_type = ANY (ARRAY['collection'::text, 'pickup'::text, 'dropoff'::text])))
);

ALTER TABLE ONLY public.shift_bins FORCE ROW LEVEL SECURITY;


--
-- Name: shift_bins_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.shift_bins_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: shift_bins_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.shift_bins_id_seq OWNED BY public.shift_bins.id;


--
-- Name: shift_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shift_history (
    id text NOT NULL,
    driver_id text NOT NULL,
    route_id text,
    start_time bigint,
    end_time bigint,
    created_at bigint NOT NULL,
    ended_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    total_pause_seconds integer DEFAULT 0,
    total_bins integer DEFAULT 0,
    completed_bins integer DEFAULT 0,
    completion_rate numeric(5,2) NOT NULL,
    end_reason text NOT NULL,
    ended_by_user_id text,
    end_reason_metadata jsonb,
    incidents_reported integer DEFAULT 0,
    field_observations integer DEFAULT 0,
    optimization_metadata jsonb,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT shift_history_end_reason_check CHECK ((end_reason = ANY (ARRAY['completed'::text, 'manual_end'::text, 'manager_ended'::text, 'manager_cancelled'::text, 'driver_disconnected'::text, 'system_timeout'::text])))
);

ALTER TABLE ONLY public.shift_history FORCE ROW LEVEL SECURITY;


--
-- Name: shift_tasks_detailed; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.shift_tasks_detailed WITH (security_invoker='on') AS
 SELECT rt.id,
    rt.shift_id,
    rt.sequence_order,
    rt.task_type,
    rt.latitude,
    rt.longitude,
    rt.address,
    rt.bin_id,
    rt.bin_number,
    rt.fill_percentage,
    rt.potential_location_id,
    rt.new_bin_number,
    rt.move_request_id,
    rt.destination_latitude,
    rt.destination_longitude,
    rt.destination_address,
    rt.move_type,
    rt.warehouse_action,
    rt.bins_to_load,
    rt.route_id,
    rt.is_completed,
    rt.completed_at,
    rt.skipped,
    rt.updated_fill_percentage,
    rt.task_data,
    rt.created_at,
    rt.updated_at,
    b.bin_number AS bin_number_from_bins,
    b.current_street,
    b.city,
    b.zip,
    pl.address AS potential_location_address,
    bmr.reason AS move_reason
   FROM (((public.route_tasks rt
     LEFT JOIN public.bins b ON ((rt.bin_id = b.id)))
     LEFT JOIN public.potential_locations pl ON ((rt.potential_location_id = pl.id)))
     LEFT JOIN public.bin_move_requests bmr ON ((rt.move_request_id = bmr.id)));


--
-- Name: shifts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.shifts (
    id text NOT NULL,
    driver_id text NOT NULL,
    route_id text,
    status text NOT NULL,
    start_time bigint,
    end_time bigint,
    total_pause_seconds integer DEFAULT 0,
    pause_start_time bigint,
    total_bins integer DEFAULT 0,
    completed_bins integer DEFAULT 0,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    warehouse_latitude numeric(10,8),
    warehouse_longitude numeric(11,8),
    warehouse_address character varying(500),
    truck_bin_capacity integer DEFAULT 6,
    optimization_metadata jsonb,
    lock_route_order boolean DEFAULT false,
    shift_label text,
    start_latitude double precision,
    start_longitude double precision,
    start_address text,
    end_latitude double precision,
    end_longitude double precision,
    end_address text,
    shift_type text DEFAULT 'standard'::text NOT NULL,
    scheduled_start timestamp with time zone,
    scheduled_end timestamp with time zone,
    preloaded_bins integer DEFAULT 0,
    ready_to_end_at bigint,
    scheduled_date date,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT shifts_check CHECK ((completed_bins <= total_bins)),
    CONSTRAINT shifts_shift_type_check CHECK ((shift_type = ANY (ARRAY['standard'::text, 'custom'::text]))),
    CONSTRAINT shifts_status_check CHECK ((status = ANY (ARRAY['inactive'::text, 'ready'::text, 'active'::text, 'paused'::text, 'ended'::text, 'cancelled'::text]))),
    CONSTRAINT shifts_total_pause_seconds_check CHECK ((total_pause_seconds >= 0))
);

ALTER TABLE ONLY public.shifts FORCE ROW LEVEL SECURITY;


--
-- Name: user_notification_preferences; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_notification_preferences (
    user_id text NOT NULL,
    drift_alerts boolean DEFAULT true NOT NULL,
    digests boolean DEFAULT true NOT NULL,
    shift_events boolean DEFAULT true NOT NULL,
    move_requests boolean DEFAULT true NOT NULL,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    overdue_move_alerts boolean DEFAULT true NOT NULL,
    due_soon_alerts boolean DEFAULT true NOT NULL,
    bin_check_reports boolean DEFAULT true NOT NULL,
    battery_alerts boolean DEFAULT true NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL
);

ALTER TABLE ONLY public.user_notification_preferences FORCE ROW LEVEL SECURITY;


--
-- Name: user_notifications; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_notifications (
    id text NOT NULL,
    user_id text NOT NULL,
    notification_log_id text,
    type text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    data jsonb,
    delivery_status text DEFAULT 'pending'::text NOT NULL,
    read_at bigint,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT user_notifications_delivery_status_check CHECK ((delivery_status = ANY (ARRAY['pending'::text, 'delivered'::text, 'failed'::text])))
);

ALTER TABLE ONLY public.user_notifications FORCE ROW LEVEL SECURITY;


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    password text NOT NULL,
    name text NOT NULL,
    role text NOT NULL,
    created_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    updated_at bigint DEFAULT (EXTRACT(epoch FROM now()))::bigint NOT NULL,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['driver'::text, 'admin'::text])))
);

ALTER TABLE ONLY public.users FORCE ROW LEVEL SECURITY;


--
-- Name: zone_incidents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zone_incidents (
    id text NOT NULL,
    zone_id text NOT NULL,
    bin_id text,
    incident_type text NOT NULL,
    reported_by_user_id text,
    reported_at bigint NOT NULL,
    description text,
    photo_url text,
    check_id integer,
    move_id integer,
    status text DEFAULT 'open'::text NOT NULL,
    shift_id text,
    reporter_latitude double precision,
    reporter_longitude double precision,
    is_field_observation boolean DEFAULT false NOT NULL,
    verified_by_user_id text,
    verified_at bigint,
    source text,
    move_request_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT zone_incidents_incident_type_check CHECK ((incident_type = ANY (ARRAY['vandalism'::text, 'landlord_complaint'::text, 'theft'::text, 'relocation_request'::text, 'missing'::text, 'damaged'::text, 'vandalized'::text, 'inaccessible'::text, 'pulled_from_service'::text]))),
    CONSTRAINT zone_incidents_source_check CHECK ((source = ANY (ARRAY['driver_shift'::text, 'manager_report'::text, 'admin_bin_change'::text, 'move_request'::text]))),
    CONSTRAINT zone_incidents_status_check CHECK ((status = ANY (ARRAY['open'::text, 'resolved'::text, 'investigating'::text])))
);

ALTER TABLE ONLY public.zone_incidents FORCE ROW LEVEL SECURITY;


--
-- Name: zone_risk_overrides; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zone_risk_overrides (
    id text NOT NULL,
    zone_id text NOT NULL,
    bin_id text NOT NULL,
    manager_id text NOT NULL,
    override_reason text NOT NULL,
    override_at bigint NOT NULL,
    expires_at bigint,
    status text DEFAULT 'active'::text NOT NULL,
    incident_count integer DEFAULT 0 NOT NULL,
    last_incident_id text,
    organization_id text DEFAULT NULLIF(current_setting('app.org_id'::text, true), ''::text) NOT NULL,
    CONSTRAINT zone_risk_overrides_status_check CHECK ((status = ANY (ARRAY['active'::text, 'expired'::text, 'revoked'::text])))
);

ALTER TABLE ONLY public.zone_risk_overrides FORCE ROW LEVEL SECURITY;


--
-- Name: zz_backup_overnight2_20260710_bins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zz_backup_overnight2_20260710_bins (
    id text,
    bin_number integer,
    current_street text,
    city text,
    zip text,
    last_moved bigint,
    last_checked bigint,
    status text,
    fill_percentage integer,
    checked integer,
    move_requested integer,
    latitude double precision,
    longitude double precision,
    created_at bigint,
    updated_at bigint,
    last_checked_at bigint,
    created_by_user_id text,
    retired_at bigint,
    retired_by_user_id text,
    placement_photo_url text,
    source_potential_location_id text
);

ALTER TABLE ONLY public.zz_backup_overnight2_20260710_bins FORCE ROW LEVEL SECURITY;


--
-- Name: zz_backup_overnight2_20260710_route_tasks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zz_backup_overnight2_20260710_route_tasks (
    id uuid,
    shift_id text,
    sequence_order integer,
    task_type character varying(20),
    latitude numeric(10,8),
    longitude numeric(11,8),
    address character varying(500),
    bin_id text,
    bin_number integer,
    fill_percentage integer,
    potential_location_id text,
    new_bin_number integer,
    move_request_id text,
    destination_latitude numeric(10,8),
    destination_longitude numeric(11,8),
    destination_address character varying(500),
    move_type character varying(50),
    warehouse_action character varying(20),
    bins_to_load integer,
    route_id character varying(100),
    is_completed integer,
    completed_at bigint,
    skipped boolean,
    updated_fill_percentage integer,
    task_data jsonb,
    created_at bigint,
    updated_at bigint,
    placement_source text,
    is_deleted boolean,
    deleted_at bigint,
    deleted_by character varying(255),
    deletion_reason text,
    added_by text,
    addition_reason text,
    earliest_arrival timestamp with time zone,
    latest_arrival timestamp with time zone,
    time_window_type text,
    service_duration_seconds integer,
    task_label text,
    task_description text,
    photo_required boolean,
    completion_notes text,
    photo_url text,
    after_photo_url text,
    photo_latitude double precision,
    photo_longitude double precision,
    after_photo_latitude double precision,
    after_photo_longitude double precision
);

ALTER TABLE ONLY public.zz_backup_overnight2_20260710_route_tasks FORCE ROW LEVEL SECURITY;


--
-- Name: zz_backup_overnight_20260710_bins; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zz_backup_overnight_20260710_bins (
    id text,
    bin_number integer,
    current_street text,
    city text,
    zip text,
    last_moved bigint,
    last_checked bigint,
    status text,
    fill_percentage integer,
    checked integer,
    move_requested integer,
    latitude double precision,
    longitude double precision,
    created_at bigint,
    updated_at bigint,
    last_checked_at bigint,
    created_by_user_id text,
    retired_at bigint,
    retired_by_user_id text,
    placement_photo_url text,
    source_potential_location_id text
);

ALTER TABLE ONLY public.zz_backup_overnight_20260710_bins FORCE ROW LEVEL SECURITY;


--
-- Name: zz_backup_overnight_20260710_route_tasks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.zz_backup_overnight_20260710_route_tasks (
    id uuid,
    shift_id text,
    sequence_order integer,
    task_type character varying(20),
    latitude numeric(10,8),
    longitude numeric(11,8),
    address character varying(500),
    bin_id text,
    bin_number integer,
    fill_percentage integer,
    potential_location_id text,
    new_bin_number integer,
    move_request_id text,
    destination_latitude numeric(10,8),
    destination_longitude numeric(11,8),
    destination_address character varying(500),
    move_type character varying(50),
    warehouse_action character varying(20),
    bins_to_load integer,
    route_id character varying(100),
    is_completed integer,
    completed_at bigint,
    skipped boolean,
    updated_fill_percentage integer,
    task_data jsonb,
    created_at bigint,
    updated_at bigint,
    placement_source text,
    is_deleted boolean,
    deleted_at bigint,
    deleted_by character varying(255),
    deletion_reason text,
    added_by text,
    addition_reason text,
    earliest_arrival timestamp with time zone,
    latest_arrival timestamp with time zone,
    time_window_type text,
    service_duration_seconds integer,
    task_label text,
    task_description text,
    photo_required boolean,
    completion_notes text,
    photo_url text,
    after_photo_url text,
    photo_latitude double precision,
    photo_longitude double precision,
    after_photo_latitude double precision,
    after_photo_longitude double precision
);

ALTER TABLE ONLY public.zz_backup_overnight_20260710_route_tasks FORCE ROW LEVEL SECURITY;


--
-- Name: checks id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks ALTER COLUMN id SET DEFAULT nextval('public.checks_id_seq'::regclass);


--
-- Name: config id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config ALTER COLUMN id SET DEFAULT nextval('public.config_id_seq'::regclass);


--
-- Name: driver_location_history id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_history ALTER COLUMN id SET DEFAULT nextval('public.driver_location_history_id_seq'::regclass);


--
-- Name: driver_location_snapshots id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_snapshots ALTER COLUMN id SET DEFAULT nextval('public.driver_location_snapshots_id_seq'::regclass);


--
-- Name: driver_locations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_locations ALTER COLUMN id SET DEFAULT nextval('public.driver_locations_id_seq'::regclass);


--
-- Name: fcm_tokens id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fcm_tokens ALTER COLUMN id SET DEFAULT nextval('public.fcm_tokens_id_seq'::regclass);


--
-- Name: move_request_history seq; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.move_request_history ALTER COLUMN seq SET DEFAULT nextval('public.move_request_history_seq_seq'::regclass);


--
-- Name: moves id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves ALTER COLUMN id SET DEFAULT nextval('public.moves_id_seq'::regclass);


--
-- Name: route_bins id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_bins ALTER COLUMN id SET DEFAULT nextval('public.route_bins_id_seq1'::regclass);


--
-- Name: shift_bins id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins ALTER COLUMN id SET DEFAULT nextval('public.shift_bins_id_seq'::regclass);


--
-- Name: ai_recommendations ai_recommendations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_recommendations
    ADD CONSTRAINT ai_recommendations_pkey PRIMARY KEY (id);


--
-- Name: airtag_accounts airtag_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_accounts
    ADD CONSTRAINT airtag_accounts_pkey PRIMARY KEY (id);


--
-- Name: airtag_keys airtag_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_keys
    ADD CONSTRAINT airtag_keys_pkey PRIMARY KEY (id);


--
-- Name: airtag_locations airtag_locations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_locations
    ADD CONSTRAINT airtag_locations_pkey PRIMARY KEY (id);


--
-- Name: app_error_logs app_error_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_error_logs
    ADD CONSTRAINT app_error_logs_pkey PRIMARY KEY (id);


--
-- Name: bin_change_log bin_change_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_change_log
    ADD CONSTRAINT bin_change_log_pkey PRIMARY KEY (id);


--
-- Name: bin_check_recommendations bin_check_recommendations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_check_recommendations
    ADD CONSTRAINT bin_check_recommendations_pkey PRIMARY KEY (id);


--
-- Name: bin_features bin_features_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_features
    ADD CONSTRAINT bin_features_pkey PRIMARY KEY (bin_id);


--
-- Name: bin_move_requests bin_move_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_pkey PRIMARY KEY (id);


--
-- Name: bin_watchlist bin_watchlist_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_watchlist
    ADD CONSTRAINT bin_watchlist_pkey PRIMARY KEY (id);


--
-- Name: bins bins_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_pkey PRIMARY KEY (id);


--
-- Name: census_income_cache census_income_cache_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.census_income_cache
    ADD CONSTRAINT census_income_cache_pkey PRIMARY KEY (zip);


--
-- Name: checks checks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT checks_pkey PRIMARY KEY (id);


--
-- Name: config config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config
    ADD CONSTRAINT config_pkey PRIMARY KEY (id);


--
-- Name: driver_current_location driver_current_location_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_current_location
    ADD CONSTRAINT driver_current_location_pkey PRIMARY KEY (driver_id);


--
-- Name: driver_location_history driver_location_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_history
    ADD CONSTRAINT driver_location_history_pkey PRIMARY KEY (id);


--
-- Name: driver_location_snapshots driver_location_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_snapshots
    ADD CONSTRAINT driver_location_snapshots_pkey PRIMARY KEY (id);


--
-- Name: driver_locations driver_locations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_locations
    ADD CONSTRAINT driver_locations_pkey PRIMARY KEY (id);


--
-- Name: fcm_tokens fcm_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fcm_tokens
    ADD CONSTRAINT fcm_tokens_pkey PRIMARY KEY (id);


--
-- Name: fcm_tokens fcm_tokens_token_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fcm_tokens
    ADD CONSTRAINT fcm_tokens_token_key UNIQUE (token);


--
-- Name: move_request_history move_request_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.move_request_history
    ADD CONSTRAINT move_request_history_pkey PRIMARY KEY (id);


--
-- Name: moves moves_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT moves_pkey PRIMARY KEY (id);


--
-- Name: no_go_zones no_go_zones_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT no_go_zones_pkey PRIMARY KEY (id);


--
-- Name: notification_log notification_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notification_log
    ADD CONSTRAINT notification_log_pkey PRIMARY KEY (id);


--
-- Name: organizations organizations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.organizations
    ADD CONSTRAINT organizations_pkey PRIMARY KEY (id);


--
-- Name: placement_decisions placement_decisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.placement_decisions
    ADD CONSTRAINT placement_decisions_pkey PRIMARY KEY (id);


--
-- Name: potential_locations potential_locations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT potential_locations_pkey PRIMARY KEY (id);


--
-- Name: route_bins route_bins_pkey1; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_bins
    ADD CONSTRAINT route_bins_pkey1 PRIMARY KEY (id);


--
-- Name: route_tasks route_tasks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT route_tasks_pkey PRIMARY KEY (id);


--
-- Name: routes routes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.routes
    ADD CONSTRAINT routes_pkey PRIMARY KEY (id);


--
-- Name: shift_bins shift_bins_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins
    ADD CONSTRAINT shift_bins_pkey PRIMARY KEY (id);


--
-- Name: shift_history shift_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_history
    ADD CONSTRAINT shift_history_pkey PRIMARY KEY (id);


--
-- Name: shifts shifts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shifts
    ADD CONSTRAINT shifts_pkey PRIMARY KEY (id);


--
-- Name: airtag_accounts uq_airtag_accounts_org_email; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_accounts
    ADD CONSTRAINT uq_airtag_accounts_org_email UNIQUE (organization_id, email);


--
-- Name: airtag_accounts uq_airtag_accounts_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_accounts
    ADD CONSTRAINT uq_airtag_accounts_org_id UNIQUE (organization_id, id);


--
-- Name: bin_move_requests uq_bin_move_requests_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT uq_bin_move_requests_org_id UNIQUE (organization_id, id);


--
-- Name: bins uq_bins_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT uq_bins_org_id UNIQUE (organization_id, id);


--
-- Name: checks uq_checks_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT uq_checks_org_id UNIQUE (organization_id, id);


--
-- Name: config uq_config_org_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config
    ADD CONSTRAINT uq_config_org_key UNIQUE (organization_id, key);


--
-- Name: moves uq_moves_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT uq_moves_org_id UNIQUE (organization_id, id);


--
-- Name: no_go_zones uq_no_go_zones_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT uq_no_go_zones_org_id UNIQUE (organization_id, id);


--
-- Name: notification_log uq_notification_log_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notification_log
    ADD CONSTRAINT uq_notification_log_org_id UNIQUE (organization_id, id);


--
-- Name: potential_locations uq_potential_locations_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT uq_potential_locations_org_id UNIQUE (organization_id, id);


--
-- Name: routes uq_routes_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.routes
    ADD CONSTRAINT uq_routes_org_id UNIQUE (organization_id, id);


--
-- Name: shifts uq_shifts_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shifts
    ADD CONSTRAINT uq_shifts_org_id UNIQUE (organization_id, id);


--
-- Name: users uq_users_org_email; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT uq_users_org_email UNIQUE (organization_id, email);


--
-- Name: users uq_users_org_id; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT uq_users_org_id UNIQUE (organization_id, id);


--
-- Name: user_notification_preferences user_notification_preferences_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notification_preferences
    ADD CONSTRAINT user_notification_preferences_pkey PRIMARY KEY (user_id);


--
-- Name: user_notifications user_notifications_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notifications
    ADD CONSTRAINT user_notifications_pkey PRIMARY KEY (id);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: zone_incidents zone_incidents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_pkey PRIMARY KEY (id);


--
-- Name: zone_risk_overrides zone_risk_overrides_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_risk_overrides
    ADD CONSTRAINT zone_risk_overrides_pkey PRIMARY KEY (id);


--
-- Name: idx_ai_recommendations_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_recommendations_created ON public.ai_recommendations USING btree (created_at DESC);


--
-- Name: idx_ai_recommendations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_recommendations_org ON public.ai_recommendations USING btree (organization_id);


--
-- Name: idx_ai_recommendations_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_recommendations_status ON public.ai_recommendations USING btree (status);


--
-- Name: idx_ai_recommendations_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ai_recommendations_type ON public.ai_recommendations USING btree (type);


--
-- Name: idx_airtag_accounts_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_accounts_org ON public.airtag_accounts USING btree (organization_id);


--
-- Name: idx_airtag_keys_account_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_keys_account_id ON public.airtag_keys USING btree (account_id);


--
-- Name: idx_airtag_keys_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_keys_org ON public.airtag_keys USING btree (organization_id);


--
-- Name: idx_airtag_keys_tag_uuid; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_keys_tag_uuid ON public.airtag_keys USING btree (tag_uuid);


--
-- Name: idx_airtag_locations_last_seen; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_locations_last_seen ON public.airtag_locations USING btree (last_seen);


--
-- Name: idx_airtag_locations_name; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_locations_name ON public.airtag_locations USING btree (name);


--
-- Name: idx_airtag_locations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_locations_org ON public.airtag_locations USING btree (organization_id);


--
-- Name: idx_airtag_locations_org_binnum; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_airtag_locations_org_binnum ON public.airtag_locations USING btree (organization_id, bin_number);


--
-- Name: idx_app_error_logs_context; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_context ON public.app_error_logs USING btree (context);


--
-- Name: idx_app_error_logs_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_created_at ON public.app_error_logs USING btree (created_at DESC);


--
-- Name: idx_app_error_logs_driver_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_driver_id ON public.app_error_logs USING btree (driver_id);


--
-- Name: idx_app_error_logs_error_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_error_type ON public.app_error_logs USING btree (error_type);


--
-- Name: idx_app_error_logs_is_resolved; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_is_resolved ON public.app_error_logs USING btree (is_resolved);


--
-- Name: idx_app_error_logs_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_org ON public.app_error_logs USING btree (organization_id);


--
-- Name: idx_app_error_logs_severity; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_severity ON public.app_error_logs USING btree (severity);


--
-- Name: idx_app_error_logs_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_app_error_logs_shift_id ON public.app_error_logs USING btree (shift_id);


--
-- Name: idx_bin_change_log_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_change_log_bin_id ON public.bin_change_log USING btree (bin_id);


--
-- Name: idx_bin_change_log_changed_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_change_log_changed_at ON public.bin_change_log USING btree (created_at DESC);


--
-- Name: idx_bin_change_log_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_change_log_org ON public.bin_change_log USING btree (organization_id);


--
-- Name: idx_bin_change_log_org_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_change_log_org_bin ON public.bin_change_log USING btree (organization_id, bin_id);


--
-- Name: idx_bin_check_recommendations_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_check_recommendations_bin_id ON public.bin_check_recommendations USING btree (bin_id);


--
-- Name: idx_bin_check_recommendations_bin_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_check_recommendations_bin_status ON public.bin_check_recommendations USING btree (bin_id, status);


--
-- Name: idx_bin_check_recommendations_flagged_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_check_recommendations_flagged_at ON public.bin_check_recommendations USING btree (flagged_at DESC);


--
-- Name: idx_bin_check_recommendations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_check_recommendations_org ON public.bin_check_recommendations USING btree (organization_id);


--
-- Name: idx_bin_check_recommendations_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_check_recommendations_status ON public.bin_check_recommendations USING btree (status);


--
-- Name: idx_bin_features_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_features_org ON public.bin_features USING btree (organization_id);


--
-- Name: idx_bin_move_requests_assigned_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_assigned_shift_id ON public.bin_move_requests USING btree (assigned_shift_id);


--
-- Name: idx_bin_move_requests_assigned_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_assigned_user ON public.bin_move_requests USING btree (assigned_user_id);


--
-- Name: idx_bin_move_requests_assigned_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_assigned_user_id ON public.bin_move_requests USING btree (assigned_user_id);


--
-- Name: idx_bin_move_requests_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_bin_id ON public.bin_move_requests USING btree (bin_id);


--
-- Name: idx_bin_move_requests_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_org ON public.bin_move_requests USING btree (organization_id);


--
-- Name: idx_bin_move_requests_scheduled_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_scheduled_date ON public.bin_move_requests USING btree (scheduled_date);


--
-- Name: idx_bin_move_requests_source_potential_location; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_source_potential_location ON public.bin_move_requests USING btree (source_potential_location_id) WHERE (source_potential_location_id IS NOT NULL);


--
-- Name: idx_bin_move_requests_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_status ON public.bin_move_requests USING btree (status);


--
-- Name: idx_bin_move_requests_urgency; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_move_requests_urgency ON public.bin_move_requests USING btree (urgency);


--
-- Name: idx_bin_watchlist_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_watchlist_org ON public.bin_watchlist USING btree (organization_id);


--
-- Name: idx_bin_watchlist_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bin_watchlist_status ON public.bin_watchlist USING btree (status, evaluate_at);


--
-- Name: idx_bins_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_org ON public.bins USING btree (organization_id);


--
-- Name: idx_bins_org_bin_number; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_org_bin_number ON public.bins USING btree (organization_id, bin_number);


--
-- Name: idx_bins_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_org_status ON public.bins USING btree (organization_id, status);


--
-- Name: idx_bins_placement_photo; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_placement_photo ON public.bins USING btree (placement_photo_url) WHERE (placement_photo_url IS NOT NULL);


--
-- Name: idx_bins_retired_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_retired_at ON public.bins USING btree (retired_at);


--
-- Name: idx_bins_source_potential_location; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_source_potential_location ON public.bins USING btree (source_potential_location_id);


--
-- Name: idx_bins_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bins_status ON public.bins USING btree (status);


--
-- Name: idx_bmr_org_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bmr_org_bin ON public.bin_move_requests USING btree (organization_id, bin_id);


--
-- Name: idx_bmr_org_scheduled_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bmr_org_scheduled_date ON public.bin_move_requests USING btree (organization_id, scheduled_date);


--
-- Name: idx_bmr_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_bmr_org_status ON public.bin_move_requests USING btree (organization_id, status);


--
-- Name: idx_checks_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_bin_id ON public.checks USING btree (bin_id);


--
-- Name: idx_checks_checked_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_checked_by ON public.checks USING btree (checked_by);


--
-- Name: idx_checks_checked_on; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_checked_on ON public.checks USING btree (checked_on);


--
-- Name: idx_checks_move_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_move_request_id ON public.checks USING btree (move_request_id);


--
-- Name: idx_checks_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_org ON public.checks USING btree (organization_id);


--
-- Name: idx_checks_org_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_org_bin ON public.checks USING btree (organization_id, bin_id);


--
-- Name: idx_checks_org_checked_on; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_org_checked_on ON public.checks USING btree (organization_id, checked_on DESC);


--
-- Name: idx_checks_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_checks_shift_id ON public.checks USING btree (shift_id);


--
-- Name: idx_config_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_config_org ON public.config USING btree (organization_id);


--
-- Name: idx_dcl_org_connected; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_dcl_org_connected ON public.driver_current_location USING btree (organization_id, is_connected);


--
-- Name: idx_driver_current_location_is_connected; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_current_location_is_connected ON public.driver_current_location USING btree (is_connected);


--
-- Name: idx_driver_current_location_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_current_location_org ON public.driver_current_location USING btree (organization_id);


--
-- Name: idx_driver_current_location_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_current_location_shift_id ON public.driver_current_location USING btree (shift_id);


--
-- Name: idx_driver_location_history_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_location_history_org ON public.driver_location_history USING btree (organization_id);


--
-- Name: idx_driver_location_snapshots_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_location_snapshots_org ON public.driver_location_snapshots USING btree (organization_id);


--
-- Name: idx_driver_locations_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_locations_created_at ON public.driver_locations USING btree (created_at);


--
-- Name: idx_driver_locations_driver_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_locations_driver_id ON public.driver_locations USING btree (driver_id);


--
-- Name: idx_driver_locations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_locations_org ON public.driver_locations USING btree (organization_id);


--
-- Name: idx_driver_locations_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_locations_shift_id ON public.driver_locations USING btree (shift_id);


--
-- Name: idx_driver_locations_timestamp; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_locations_timestamp ON public.driver_locations USING btree ("timestamp");


--
-- Name: idx_driver_snapshots_driver; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_driver_snapshots_driver ON public.driver_location_snapshots USING btree (driver_id, recorded_at DESC);


--
-- Name: idx_fcm_tokens_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_fcm_tokens_org ON public.fcm_tokens USING btree (organization_id);


--
-- Name: idx_fcm_tokens_org_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_fcm_tokens_org_user ON public.fcm_tokens USING btree (organization_id, user_id);


--
-- Name: idx_fcm_tokens_token; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_fcm_tokens_token ON public.fcm_tokens USING btree (token);


--
-- Name: idx_fcm_tokens_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_fcm_tokens_user_id ON public.fcm_tokens USING btree (user_id);


--
-- Name: idx_location_history_driver_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_location_history_driver_time ON public.driver_location_history USING btree (driver_id, recorded_at DESC);


--
-- Name: idx_location_history_recorded_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_location_history_recorded_at ON public.driver_location_history USING btree (recorded_at DESC);


--
-- Name: idx_location_history_shift; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_location_history_shift ON public.driver_location_history USING btree (shift_id) WHERE (shift_id IS NOT NULL);


--
-- Name: idx_move_request_history_action_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_action_type ON public.move_request_history USING btree (action_type);


--
-- Name: idx_move_request_history_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_actor ON public.move_request_history USING btree (actor_id);


--
-- Name: idx_move_request_history_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_created_at ON public.move_request_history USING btree (created_at);


--
-- Name: idx_move_request_history_move_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_move_id ON public.move_request_history USING btree (move_request_id);


--
-- Name: idx_move_request_history_move_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_move_time ON public.move_request_history USING btree (move_request_id, created_at DESC);


--
-- Name: idx_move_request_history_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_move_request_history_org ON public.move_request_history USING btree (organization_id);


--
-- Name: idx_moves_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_bin_id ON public.moves USING btree (bin_id);


--
-- Name: idx_moves_completed_by_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_completed_by_user_id ON public.moves USING btree (completed_by_user_id);


--
-- Name: idx_moves_move_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_move_request_id ON public.moves USING btree (move_request_id);


--
-- Name: idx_moves_move_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_move_type ON public.moves USING btree (move_type);


--
-- Name: idx_moves_moved_on; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_moved_on ON public.moves USING btree (moved_on);


--
-- Name: idx_moves_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_org ON public.moves USING btree (organization_id);


--
-- Name: idx_moves_org_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_org_bin ON public.moves USING btree (organization_id, bin_id);


--
-- Name: idx_moves_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_moves_shift_id ON public.moves USING btree (shift_id);


--
-- Name: idx_mrh_move_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_mrh_move_seq ON public.move_request_history USING btree (move_request_id, seq);


--
-- Name: idx_mrh_org_move_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_mrh_org_move_seq ON public.move_request_history USING btree (organization_id, move_request_id, seq);


--
-- Name: idx_no_go_zones_created_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_created_by ON public.no_go_zones USING btree (created_by_user_id);


--
-- Name: idx_no_go_zones_location; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_location ON public.no_go_zones USING btree (center_latitude, center_longitude);


--
-- Name: idx_no_go_zones_merged_into; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_merged_into ON public.no_go_zones USING btree (merged_into_zone_id);


--
-- Name: idx_no_go_zones_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_org ON public.no_go_zones USING btree (organization_id);


--
-- Name: idx_no_go_zones_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_org_status ON public.no_go_zones USING btree (organization_id, status);


--
-- Name: idx_no_go_zones_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_no_go_zones_status ON public.no_go_zones USING btree (status);


--
-- Name: idx_notification_log_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notification_log_created_at ON public.notification_log USING btree (created_at DESC);


--
-- Name: idx_notification_log_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notification_log_org ON public.notification_log USING btree (organization_id);


--
-- Name: idx_notification_log_org_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notification_log_org_created ON public.notification_log USING btree (organization_id, created_at DESC);


--
-- Name: idx_notification_log_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_notification_log_type ON public.notification_log USING btree (type);


--
-- Name: idx_placement_decisions_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_placement_decisions_org ON public.placement_decisions USING btree (organization_id);


--
-- Name: idx_potential_loc_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_loc_org_status ON public.potential_locations USING btree (organization_id, conversion_status);


--
-- Name: idx_potential_locations_active; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_active ON public.potential_locations USING btree (created_at DESC) WHERE (converted_at IS NULL);


--
-- Name: idx_potential_locations_assigned_shift; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_assigned_shift ON public.potential_locations USING btree (assigned_shift_id);


--
-- Name: idx_potential_locations_bin_current_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_bin_current_status ON public.potential_locations USING btree (bin_current_status);


--
-- Name: idx_potential_locations_conversion_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_conversion_status ON public.potential_locations USING btree (conversion_status);


--
-- Name: idx_potential_locations_converted; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_converted ON public.potential_locations USING btree (converted_to_bin_id) WHERE (converted_to_bin_id IS NOT NULL);


--
-- Name: idx_potential_locations_converted_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_converted_at ON public.potential_locations USING btree (converted_at DESC);


--
-- Name: idx_potential_locations_converted_via_shift; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_converted_via_shift ON public.potential_locations USING btree (converted_via_shift_id) WHERE (converted_via_shift_id IS NOT NULL);


--
-- Name: idx_potential_locations_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_created_at ON public.potential_locations USING btree (created_at DESC);


--
-- Name: idx_potential_locations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_org ON public.potential_locations USING btree (organization_id);


--
-- Name: idx_potential_locations_requested_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_requested_by ON public.potential_locations USING btree (requested_by_user_id);


--
-- Name: idx_potential_locations_shift_conversion; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_potential_locations_shift_conversion ON public.potential_locations USING btree (converted_via_shift_id) WHERE (converted_via_shift_id IS NOT NULL);


--
-- Name: idx_route_bins_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_bins_org ON public.route_bins USING btree (organization_id);


--
-- Name: idx_route_bins_route_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_bins_route_id ON public.route_bins USING btree (route_id);


--
-- Name: idx_route_bins_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX idx_route_bins_unique ON public.route_bins USING btree (organization_id, route_id, bin_id);


--
-- Name: idx_route_tasks_added_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_added_by ON public.route_tasks USING btree (added_by) WHERE (added_by IS NOT NULL);


--
-- Name: idx_route_tasks_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_bin_id ON public.route_tasks USING btree (bin_id) WHERE (bin_id IS NOT NULL);


--
-- Name: idx_route_tasks_completed; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_completed ON public.route_tasks USING btree (is_completed);


--
-- Name: idx_route_tasks_move_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_move_request_id ON public.route_tasks USING btree (move_request_id) WHERE (move_request_id IS NOT NULL);


--
-- Name: idx_route_tasks_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_org ON public.route_tasks USING btree (organization_id);


--
-- Name: idx_route_tasks_org_move_request; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_org_move_request ON public.route_tasks USING btree (organization_id, move_request_id) WHERE (move_request_id IS NOT NULL);


--
-- Name: idx_route_tasks_org_shift_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_org_shift_seq ON public.route_tasks USING btree (organization_id, shift_id, is_deleted, sequence_order);


--
-- Name: idx_route_tasks_sequence; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_sequence ON public.route_tasks USING btree (shift_id, sequence_order);


--
-- Name: idx_route_tasks_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_shift_id ON public.route_tasks USING btree (shift_id);


--
-- Name: idx_route_tasks_shift_id_deleted; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_shift_id_deleted ON public.route_tasks USING btree (shift_id, is_deleted, sequence_order);


--
-- Name: idx_route_tasks_shift_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_shift_seq ON public.route_tasks USING btree (shift_id, sequence_order);


--
-- Name: idx_route_tasks_task_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_route_tasks_task_type ON public.route_tasks USING btree (task_type);


--
-- Name: idx_routes_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_routes_created_at ON public.routes USING btree (created_at);


--
-- Name: idx_routes_created_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_routes_created_by ON public.routes USING btree (created_by_user_id);


--
-- Name: idx_routes_geographic_area; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_routes_geographic_area ON public.routes USING btree (geographic_area);


--
-- Name: idx_routes_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_routes_org ON public.routes USING btree (organization_id);


--
-- Name: idx_shift_bins_bin_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_bin_id ON public.shift_bins USING btree (bin_id);


--
-- Name: idx_shift_bins_move_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_move_request_id ON public.shift_bins USING btree (move_request_id);


--
-- Name: idx_shift_bins_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_org ON public.shift_bins USING btree (organization_id);


--
-- Name: idx_shift_bins_shift_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_shift_id ON public.shift_bins USING btree (shift_id);


--
-- Name: idx_shift_bins_shift_seq; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_shift_seq ON public.shift_bins USING btree (shift_id, sequence_order);


--
-- Name: idx_shift_bins_stop_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_bins_stop_type ON public.shift_bins USING btree (stop_type);


--
-- Name: idx_shift_history_completion_rate; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_completion_rate ON public.shift_history USING btree (completion_rate);


--
-- Name: idx_shift_history_driver_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_driver_id ON public.shift_history USING btree (driver_id);


--
-- Name: idx_shift_history_end_reason; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_end_reason ON public.shift_history USING btree (end_reason);


--
-- Name: idx_shift_history_ended_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_ended_at ON public.shift_history USING btree (ended_at);


--
-- Name: idx_shift_history_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_org ON public.shift_history USING btree (organization_id);


--
-- Name: idx_shift_history_org_ended; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_org_ended ON public.shift_history USING btree (organization_id, ended_at DESC);


--
-- Name: idx_shift_history_route_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shift_history_route_id ON public.shift_history USING btree (route_id);


--
-- Name: idx_shifts_created_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_created_at ON public.shifts USING btree (created_at);


--
-- Name: idx_shifts_driver_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_driver_id ON public.shifts USING btree (driver_id);


--
-- Name: idx_shifts_optimization_metadata; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_optimization_metadata ON public.shifts USING gin (optimization_metadata) WHERE (optimization_metadata IS NOT NULL);


--
-- Name: idx_shifts_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_org ON public.shifts USING btree (organization_id);


--
-- Name: idx_shifts_org_driver; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_org_driver ON public.shifts USING btree (organization_id, driver_id);


--
-- Name: idx_shifts_org_scheduled_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_org_scheduled_date ON public.shifts USING btree (organization_id, scheduled_date);


--
-- Name: idx_shifts_org_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_org_status ON public.shifts USING btree (organization_id, status);


--
-- Name: idx_shifts_scheduled_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_scheduled_date ON public.shifts USING btree (scheduled_date);


--
-- Name: idx_shifts_shift_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_shift_type ON public.shifts USING btree (shift_type);


--
-- Name: idx_shifts_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_shifts_status ON public.shifts USING btree (status);


--
-- Name: idx_user_notification_preferences_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notification_preferences_org ON public.user_notification_preferences USING btree (organization_id);


--
-- Name: idx_user_notifications_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notifications_org ON public.user_notifications USING btree (organization_id);


--
-- Name: idx_user_notifs_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notifs_created ON public.user_notifications USING btree (user_id, created_at DESC);


--
-- Name: idx_user_notifs_org_user_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notifs_org_user_created ON public.user_notifications USING btree (organization_id, user_id, created_at DESC);


--
-- Name: idx_user_notifs_unread; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notifs_unread ON public.user_notifications USING btree (user_id, read_at) WHERE (read_at IS NULL);


--
-- Name: idx_user_notifs_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_user_notifs_user_id ON public.user_notifications USING btree (user_id);


--
-- Name: idx_users_email; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_email ON public.users USING btree (email);


--
-- Name: idx_users_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_org ON public.users USING btree (organization_id);


--
-- Name: idx_users_org_role; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_users_org_role ON public.users USING btree (organization_id, role);


--
-- Name: idx_zone_incidents_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_bin ON public.zone_incidents USING btree (bin_id);


--
-- Name: idx_zone_incidents_bin_zone; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_bin_zone ON public.zone_incidents USING btree (bin_id, zone_id);


--
-- Name: idx_zone_incidents_date; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_date ON public.zone_incidents USING btree (reported_at);


--
-- Name: idx_zone_incidents_field_observation; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_field_observation ON public.zone_incidents USING btree (is_field_observation);


--
-- Name: idx_zone_incidents_move_request_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_move_request_id ON public.zone_incidents USING btree (move_request_id);


--
-- Name: idx_zone_incidents_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_org ON public.zone_incidents USING btree (organization_id);


--
-- Name: idx_zone_incidents_org_zone; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_org_zone ON public.zone_incidents USING btree (organization_id, zone_id);


--
-- Name: idx_zone_incidents_shift; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_shift ON public.zone_incidents USING btree (shift_id);


--
-- Name: idx_zone_incidents_source; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_source ON public.zone_incidents USING btree (source);


--
-- Name: idx_zone_incidents_type; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_type ON public.zone_incidents USING btree (incident_type);


--
-- Name: idx_zone_incidents_verification; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_verification ON public.zone_incidents USING btree (verified_by_user_id, verified_at);


--
-- Name: idx_zone_incidents_zone; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_incidents_zone ON public.zone_incidents USING btree (zone_id);


--
-- Name: idx_zone_risk_overrides_bin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_risk_overrides_bin ON public.zone_risk_overrides USING btree (bin_id);


--
-- Name: idx_zone_risk_overrides_manager; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_risk_overrides_manager ON public.zone_risk_overrides USING btree (manager_id);


--
-- Name: idx_zone_risk_overrides_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_risk_overrides_org ON public.zone_risk_overrides USING btree (organization_id);


--
-- Name: idx_zone_risk_overrides_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_risk_overrides_status ON public.zone_risk_overrides USING btree (status);


--
-- Name: idx_zone_risk_overrides_zone; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_zone_risk_overrides_zone ON public.zone_risk_overrides USING btree (zone_id);


--
-- Name: uidx_bin_move_requests_one_open; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uidx_bin_move_requests_one_open ON public.bin_move_requests USING btree (organization_id, bin_id) WHERE (status = ANY (ARRAY['pending'::text, 'assigned'::text, 'in_progress'::text]));


--
-- Name: uidx_organizations_slug; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uidx_organizations_slug ON public.organizations USING btree (slug);


--
-- Name: route_tasks route_tasks_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER route_tasks_updated_at BEFORE UPDATE ON public.route_tasks FOR EACH ROW EXECUTE FUNCTION public.update_route_task_timestamp();


--
-- Name: airtag_keys airtag_keys_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_keys
    ADD CONSTRAINT airtag_keys_account_id_fkey FOREIGN KEY (organization_id, account_id) REFERENCES public.airtag_accounts(organization_id, id) ON DELETE CASCADE;


--
-- Name: app_error_logs app_error_logs_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_error_logs
    ADD CONSTRAINT app_error_logs_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (driver_id);


--
-- Name: app_error_logs app_error_logs_resolved_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_error_logs
    ADD CONSTRAINT app_error_logs_resolved_by_user_id_fkey FOREIGN KEY (organization_id, resolved_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (resolved_by_user_id);


--
-- Name: app_error_logs app_error_logs_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_error_logs
    ADD CONSTRAINT app_error_logs_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: bin_change_log bin_change_log_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_change_log
    ADD CONSTRAINT bin_change_log_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: bin_change_log bin_change_log_changed_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_change_log
    ADD CONSTRAINT bin_change_log_changed_by_user_id_fkey FOREIGN KEY (organization_id, changed_by_user_id) REFERENCES public.users(organization_id, id);


--
-- Name: bin_change_log bin_change_log_no_go_zone_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_change_log
    ADD CONSTRAINT bin_change_log_no_go_zone_id_fkey FOREIGN KEY (organization_id, no_go_zone_id) REFERENCES public.no_go_zones(organization_id, id) ON DELETE SET NULL (no_go_zone_id);


--
-- Name: bin_check_recommendations bin_check_recommendations_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_check_recommendations
    ADD CONSTRAINT bin_check_recommendations_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: bin_check_recommendations bin_check_recommendations_resolved_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_check_recommendations
    ADD CONSTRAINT bin_check_recommendations_resolved_by_user_id_fkey FOREIGN KEY (organization_id, resolved_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (resolved_by_user_id);


--
-- Name: bin_move_requests bin_move_requests_assigned_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_assigned_shift_id_fkey FOREIGN KEY (organization_id, assigned_shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (assigned_shift_id);


--
-- Name: bin_move_requests bin_move_requests_assigned_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_assigned_user_id_fkey FOREIGN KEY (organization_id, assigned_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (assigned_user_id);


--
-- Name: bin_move_requests bin_move_requests_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: bin_move_requests bin_move_requests_no_go_zone_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_no_go_zone_id_fkey FOREIGN KEY (organization_id, no_go_zone_id) REFERENCES public.no_go_zones(organization_id, id) ON DELETE SET NULL (no_go_zone_id);


--
-- Name: bin_move_requests bin_move_requests_requested_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_requested_by_fkey FOREIGN KEY (organization_id, requested_by) REFERENCES public.users(organization_id, id);


--
-- Name: bin_move_requests bin_move_requests_source_potential_location_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT bin_move_requests_source_potential_location_id_fkey FOREIGN KEY (organization_id, source_potential_location_id) REFERENCES public.potential_locations(organization_id, id);


--
-- Name: bin_watchlist bin_watchlist_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_watchlist
    ADD CONSTRAINT bin_watchlist_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: checks checks_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT checks_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: checks checks_checked_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT checks_checked_by_fkey FOREIGN KEY (organization_id, checked_by) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (checked_by);


--
-- Name: checks checks_move_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT checks_move_request_id_fkey FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE SET NULL (move_request_id);


--
-- Name: checks checks_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT checks_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: driver_current_location driver_current_location_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_current_location
    ADD CONSTRAINT driver_current_location_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: driver_current_location driver_current_location_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_current_location
    ADD CONSTRAINT driver_current_location_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: driver_location_snapshots driver_location_snapshots_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_snapshots
    ADD CONSTRAINT driver_location_snapshots_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: driver_location_snapshots driver_location_snapshots_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_snapshots
    ADD CONSTRAINT driver_location_snapshots_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: driver_locations driver_locations_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_locations
    ADD CONSTRAINT driver_locations_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: driver_locations driver_locations_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_locations
    ADD CONSTRAINT driver_locations_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: fcm_tokens fcm_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fcm_tokens
    ADD CONSTRAINT fcm_tokens_user_id_fkey FOREIGN KEY (organization_id, user_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: ai_recommendations fk_ai_recommendations_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_recommendations
    ADD CONSTRAINT fk_ai_recommendations_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: airtag_accounts fk_airtag_accounts_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_accounts
    ADD CONSTRAINT fk_airtag_accounts_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: airtag_keys fk_airtag_keys_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_keys
    ADD CONSTRAINT fk_airtag_keys_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: airtag_locations fk_airtag_locations_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.airtag_locations
    ADD CONSTRAINT fk_airtag_locations_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: app_error_logs fk_app_error_logs_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_error_logs
    ADD CONSTRAINT fk_app_error_logs_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bin_change_log fk_bin_change_log_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_change_log
    ADD CONSTRAINT fk_bin_change_log_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bin_check_recommendations fk_bin_check_recommendations_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_check_recommendations
    ADD CONSTRAINT fk_bin_check_recommendations_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bin_features fk_bin_features_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_features
    ADD CONSTRAINT fk_bin_features_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bin_move_requests fk_bin_move_requests_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_move_requests
    ADD CONSTRAINT fk_bin_move_requests_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bin_watchlist fk_bin_watchlist_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bin_watchlist
    ADD CONSTRAINT fk_bin_watchlist_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bins fk_bins_created_by; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT fk_bins_created_by FOREIGN KEY (organization_id, created_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (created_by_user_id);


--
-- Name: bins fk_bins_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT fk_bins_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: bins fk_bins_retired_by; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT fk_bins_retired_by FOREIGN KEY (organization_id, retired_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (retired_by_user_id);


--
-- Name: bins fk_bins_source_potential_location; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT fk_bins_source_potential_location FOREIGN KEY (organization_id, source_potential_location_id) REFERENCES public.potential_locations(organization_id, id) ON DELETE SET NULL (source_potential_location_id);


--
-- Name: checks fk_checks_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.checks
    ADD CONSTRAINT fk_checks_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: config fk_config_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config
    ADD CONSTRAINT fk_config_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: driver_current_location fk_driver_current_location_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_current_location
    ADD CONSTRAINT fk_driver_current_location_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: driver_location_history fk_driver_location_history_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_history
    ADD CONSTRAINT fk_driver_location_history_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: driver_location_snapshots fk_driver_location_snapshots_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_location_snapshots
    ADD CONSTRAINT fk_driver_location_snapshots_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: driver_locations fk_driver_locations_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.driver_locations
    ADD CONSTRAINT fk_driver_locations_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: fcm_tokens fk_fcm_tokens_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fcm_tokens
    ADD CONSTRAINT fk_fcm_tokens_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: move_request_history fk_move_request_history_actor; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.move_request_history
    ADD CONSTRAINT fk_move_request_history_actor FOREIGN KEY (organization_id, actor_id) REFERENCES public.users(organization_id, id);


--
-- Name: move_request_history fk_move_request_history_move_request; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.move_request_history
    ADD CONSTRAINT fk_move_request_history_move_request FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE CASCADE;


--
-- Name: move_request_history fk_move_request_history_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.move_request_history
    ADD CONSTRAINT fk_move_request_history_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: moves fk_moves_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT fk_moves_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: no_go_zones fk_no_go_zones_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT fk_no_go_zones_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: notification_log fk_notification_log_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.notification_log
    ADD CONSTRAINT fk_notification_log_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: placement_decisions fk_placement_decisions_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.placement_decisions
    ADD CONSTRAINT fk_placement_decisions_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: potential_locations fk_potential_locations_assigned_shift; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT fk_potential_locations_assigned_shift FOREIGN KEY (organization_id, assigned_shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (assigned_shift_id);


--
-- Name: potential_locations fk_potential_locations_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT fk_potential_locations_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: route_bins fk_route_bins_bin; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_bins
    ADD CONSTRAINT fk_route_bins_bin FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: route_bins fk_route_bins_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_bins
    ADD CONSTRAINT fk_route_bins_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: route_bins fk_route_bins_route; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_bins
    ADD CONSTRAINT fk_route_bins_route FOREIGN KEY (organization_id, route_id) REFERENCES public.routes(organization_id, id) ON DELETE CASCADE;


--
-- Name: route_tasks fk_route_tasks_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT fk_route_tasks_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: routes fk_routes_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.routes
    ADD CONSTRAINT fk_routes_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: routes fk_routes_user; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.routes
    ADD CONSTRAINT fk_routes_user FOREIGN KEY (organization_id, created_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (created_by_user_id);


--
-- Name: shift_bins fk_shift_bins_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins
    ADD CONSTRAINT fk_shift_bins_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: shift_history fk_shift_history_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_history
    ADD CONSTRAINT fk_shift_history_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: shifts fk_shifts_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shifts
    ADD CONSTRAINT fk_shifts_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: user_notification_preferences fk_user_notification_preferences_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notification_preferences
    ADD CONSTRAINT fk_user_notification_preferences_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: user_notifications fk_user_notifications_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notifications
    ADD CONSTRAINT fk_user_notifications_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: users fk_users_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT fk_users_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: zone_incidents fk_zone_incidents_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT fk_zone_incidents_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: zone_risk_overrides fk_zone_risk_overrides_organization; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_risk_overrides
    ADD CONSTRAINT fk_zone_risk_overrides_organization FOREIGN KEY (organization_id) REFERENCES public.organizations(id) ON DELETE RESTRICT;


--
-- Name: moves moves_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT moves_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: moves moves_completed_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT moves_completed_by_user_id_fkey FOREIGN KEY (organization_id, completed_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (completed_by_user_id);


--
-- Name: moves moves_move_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT moves_move_request_id_fkey FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE SET NULL (move_request_id);


--
-- Name: moves moves_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.moves
    ADD CONSTRAINT moves_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: no_go_zones no_go_zones_created_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT no_go_zones_created_by_user_id_fkey FOREIGN KEY (organization_id, created_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (created_by_user_id);


--
-- Name: no_go_zones no_go_zones_merged_into_zone_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT no_go_zones_merged_into_zone_id_fkey FOREIGN KEY (organization_id, merged_into_zone_id) REFERENCES public.no_go_zones(organization_id, id) ON DELETE SET NULL (merged_into_zone_id);


--
-- Name: no_go_zones no_go_zones_resolved_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.no_go_zones
    ADD CONSTRAINT no_go_zones_resolved_by_user_id_fkey FOREIGN KEY (organization_id, resolved_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (resolved_by_user_id);


--
-- Name: potential_locations potential_locations_converted_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT potential_locations_converted_by_user_id_fkey FOREIGN KEY (organization_id, converted_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (converted_by_user_id);


--
-- Name: potential_locations potential_locations_converted_to_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT potential_locations_converted_to_bin_id_fkey FOREIGN KEY (organization_id, converted_to_bin_id) REFERENCES public.bins(organization_id, id) ON DELETE SET NULL (converted_to_bin_id);


--
-- Name: potential_locations potential_locations_converted_via_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT potential_locations_converted_via_shift_id_fkey FOREIGN KEY (organization_id, converted_via_shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (converted_via_shift_id);


--
-- Name: potential_locations potential_locations_requested_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.potential_locations
    ADD CONSTRAINT potential_locations_requested_by_user_id_fkey FOREIGN KEY (organization_id, requested_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: route_tasks route_tasks_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT route_tasks_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE SET NULL (bin_id);


--
-- Name: route_tasks route_tasks_move_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT route_tasks_move_request_id_fkey FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE SET NULL (move_request_id);


--
-- Name: route_tasks route_tasks_potential_location_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT route_tasks_potential_location_id_fkey FOREIGN KEY (organization_id, potential_location_id) REFERENCES public.potential_locations(organization_id, id) ON DELETE SET NULL (potential_location_id);


--
-- Name: route_tasks route_tasks_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.route_tasks
    ADD CONSTRAINT route_tasks_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE CASCADE;


--
-- Name: shift_bins shift_bins_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins
    ADD CONSTRAINT shift_bins_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: shift_bins shift_bins_move_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins
    ADD CONSTRAINT shift_bins_move_request_id_fkey FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE SET NULL (move_request_id);


--
-- Name: shift_bins shift_bins_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_bins
    ADD CONSTRAINT shift_bins_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE CASCADE;


--
-- Name: shift_history shift_history_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_history
    ADD CONSTRAINT shift_history_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: shift_history shift_history_ended_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shift_history
    ADD CONSTRAINT shift_history_ended_by_user_id_fkey FOREIGN KEY (organization_id, ended_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (ended_by_user_id);


--
-- Name: shifts shifts_driver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.shifts
    ADD CONSTRAINT shifts_driver_id_fkey FOREIGN KEY (organization_id, driver_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: user_notification_preferences user_notification_preferences_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notification_preferences
    ADD CONSTRAINT user_notification_preferences_user_id_fkey FOREIGN KEY (organization_id, user_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: user_notifications user_notifications_notification_log_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notifications
    ADD CONSTRAINT user_notifications_notification_log_id_fkey FOREIGN KEY (organization_id, notification_log_id) REFERENCES public.notification_log(organization_id, id) ON DELETE SET NULL (notification_log_id);


--
-- Name: user_notifications user_notifications_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_notifications
    ADD CONSTRAINT user_notifications_user_id_fkey FOREIGN KEY (organization_id, user_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: zone_incidents zone_incidents_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: zone_incidents zone_incidents_check_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_check_id_fkey FOREIGN KEY (organization_id, check_id) REFERENCES public.checks(organization_id, id) ON DELETE SET NULL (check_id);


--
-- Name: zone_incidents zone_incidents_move_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_move_id_fkey FOREIGN KEY (organization_id, move_id) REFERENCES public.moves(organization_id, id) ON DELETE SET NULL (move_id);


--
-- Name: zone_incidents zone_incidents_move_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_move_request_id_fkey FOREIGN KEY (organization_id, move_request_id) REFERENCES public.bin_move_requests(organization_id, id) ON DELETE SET NULL (move_request_id);


--
-- Name: zone_incidents zone_incidents_reported_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_reported_by_user_id_fkey FOREIGN KEY (organization_id, reported_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (reported_by_user_id);


--
-- Name: zone_incidents zone_incidents_shift_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_shift_id_fkey FOREIGN KEY (organization_id, shift_id) REFERENCES public.shifts(organization_id, id) ON DELETE SET NULL (shift_id);


--
-- Name: zone_incidents zone_incidents_verified_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_verified_by_user_id_fkey FOREIGN KEY (organization_id, verified_by_user_id) REFERENCES public.users(organization_id, id) ON DELETE SET NULL (verified_by_user_id);


--
-- Name: zone_incidents zone_incidents_zone_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_incidents
    ADD CONSTRAINT zone_incidents_zone_id_fkey FOREIGN KEY (organization_id, zone_id) REFERENCES public.no_go_zones(organization_id, id) ON DELETE CASCADE;


--
-- Name: zone_risk_overrides zone_risk_overrides_bin_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_risk_overrides
    ADD CONSTRAINT zone_risk_overrides_bin_id_fkey FOREIGN KEY (organization_id, bin_id) REFERENCES public.bins(organization_id, id) ON DELETE CASCADE;


--
-- Name: zone_risk_overrides zone_risk_overrides_manager_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_risk_overrides
    ADD CONSTRAINT zone_risk_overrides_manager_id_fkey FOREIGN KEY (organization_id, manager_id) REFERENCES public.users(organization_id, id) ON DELETE CASCADE;


--
-- Name: zone_risk_overrides zone_risk_overrides_zone_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.zone_risk_overrides
    ADD CONSTRAINT zone_risk_overrides_zone_id_fkey FOREIGN KEY (organization_id, zone_id) REFERENCES public.no_go_zones(organization_id, id) ON DELETE CASCADE;


--
-- Name: ai_recommendations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.ai_recommendations ENABLE ROW LEVEL SECURITY;

--
-- Name: airtag_accounts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.airtag_accounts ENABLE ROW LEVEL SECURITY;

--
-- Name: airtag_keys; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.airtag_keys ENABLE ROW LEVEL SECURITY;

--
-- Name: airtag_locations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.airtag_locations ENABLE ROW LEVEL SECURITY;

--
-- Name: app_error_logs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.app_error_logs ENABLE ROW LEVEL SECURITY;

--
-- Name: bin_change_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bin_change_log ENABLE ROW LEVEL SECURITY;

--
-- Name: bin_check_recommendations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bin_check_recommendations ENABLE ROW LEVEL SECURITY;

--
-- Name: bin_features; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bin_features ENABLE ROW LEVEL SECURITY;

--
-- Name: bin_move_requests; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bin_move_requests ENABLE ROW LEVEL SECURITY;

--
-- Name: bin_watchlist; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bin_watchlist ENABLE ROW LEVEL SECURITY;

--
-- Name: bins; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.bins ENABLE ROW LEVEL SECURITY;

--
-- Name: checks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.checks ENABLE ROW LEVEL SECURITY;

--
-- Name: config; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.config ENABLE ROW LEVEL SECURITY;

--
-- Name: driver_current_location; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.driver_current_location ENABLE ROW LEVEL SECURITY;

--
-- Name: driver_location_history; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.driver_location_history ENABLE ROW LEVEL SECURITY;

--
-- Name: driver_location_snapshots; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.driver_location_snapshots ENABLE ROW LEVEL SECURITY;

--
-- Name: driver_locations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.driver_locations ENABLE ROW LEVEL SECURITY;

--
-- Name: fcm_tokens; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.fcm_tokens ENABLE ROW LEVEL SECURITY;

--
-- Name: move_request_history; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.move_request_history ENABLE ROW LEVEL SECURITY;

--
-- Name: moves; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.moves ENABLE ROW LEVEL SECURITY;

--
-- Name: no_go_zones; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.no_go_zones ENABLE ROW LEVEL SECURITY;

--
-- Name: notification_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.notification_log ENABLE ROW LEVEL SECURITY;

--
-- Name: organizations org_catalog_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_catalog_read ON public.organizations FOR SELECT USING (true);


--
-- Name: ai_recommendations org_isolation_ai_recommendations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_ai_recommendations ON public.ai_recommendations USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: airtag_accounts org_isolation_airtag_accounts; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_airtag_accounts ON public.airtag_accounts USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: airtag_keys org_isolation_airtag_keys; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_airtag_keys ON public.airtag_keys USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: airtag_locations org_isolation_airtag_locations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_airtag_locations ON public.airtag_locations USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: app_error_logs org_isolation_app_error_logs; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_app_error_logs ON public.app_error_logs USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bin_change_log org_isolation_bin_change_log; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bin_change_log ON public.bin_change_log USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bin_check_recommendations org_isolation_bin_check_recommendations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bin_check_recommendations ON public.bin_check_recommendations USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bin_features org_isolation_bin_features; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bin_features ON public.bin_features USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bin_move_requests org_isolation_bin_move_requests; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bin_move_requests ON public.bin_move_requests USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bin_watchlist org_isolation_bin_watchlist; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bin_watchlist ON public.bin_watchlist USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: bins org_isolation_bins; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_bins ON public.bins USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: checks org_isolation_checks; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_checks ON public.checks USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: config org_isolation_config; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_config ON public.config USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: driver_current_location org_isolation_driver_current_location; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_driver_current_location ON public.driver_current_location USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: driver_location_history org_isolation_driver_location_history; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_driver_location_history ON public.driver_location_history USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: driver_location_snapshots org_isolation_driver_location_snapshots; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_driver_location_snapshots ON public.driver_location_snapshots USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: driver_locations org_isolation_driver_locations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_driver_locations ON public.driver_locations USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: fcm_tokens org_isolation_fcm_tokens; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_fcm_tokens ON public.fcm_tokens USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: move_request_history org_isolation_move_request_history; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_move_request_history ON public.move_request_history USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: moves org_isolation_moves; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_moves ON public.moves USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: no_go_zones org_isolation_no_go_zones; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_no_go_zones ON public.no_go_zones USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: notification_log org_isolation_notification_log; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_notification_log ON public.notification_log USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: organizations org_isolation_organizations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_organizations ON public.organizations USING ((id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: placement_decisions org_isolation_placement_decisions; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_placement_decisions ON public.placement_decisions USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: potential_locations org_isolation_potential_locations; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_potential_locations ON public.potential_locations USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: route_bins org_isolation_route_bins; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_route_bins ON public.route_bins USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: route_tasks org_isolation_route_tasks; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_route_tasks ON public.route_tasks USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: routes org_isolation_routes; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_routes ON public.routes USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: shift_bins org_isolation_shift_bins; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_shift_bins ON public.shift_bins USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: shift_history org_isolation_shift_history; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_shift_history ON public.shift_history USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: shifts org_isolation_shifts; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_shifts ON public.shifts USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: user_notification_preferences org_isolation_user_notification_preferences; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_user_notification_preferences ON public.user_notification_preferences USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: user_notifications org_isolation_user_notifications; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_user_notifications ON public.user_notifications USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: users org_isolation_users; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_users ON public.users USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: zone_incidents org_isolation_zone_incidents; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_zone_incidents ON public.zone_incidents USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: zone_risk_overrides org_isolation_zone_risk_overrides; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY org_isolation_zone_risk_overrides ON public.zone_risk_overrides USING ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text))) WITH CHECK ((organization_id = NULLIF(current_setting('app.org_id'::text, true), ''::text)));


--
-- Name: organizations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.organizations ENABLE ROW LEVEL SECURITY;

--
-- Name: placement_decisions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.placement_decisions ENABLE ROW LEVEL SECURITY;

--
-- Name: potential_locations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.potential_locations ENABLE ROW LEVEL SECURITY;

--
-- Name: route_bins; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.route_bins ENABLE ROW LEVEL SECURITY;

--
-- Name: route_tasks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.route_tasks ENABLE ROW LEVEL SECURITY;

--
-- Name: routes; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.routes ENABLE ROW LEVEL SECURITY;

--
-- Name: shift_bins; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.shift_bins ENABLE ROW LEVEL SECURITY;

--
-- Name: shift_history; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.shift_history ENABLE ROW LEVEL SECURITY;

--
-- Name: shifts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.shifts ENABLE ROW LEVEL SECURITY;

--
-- Name: user_notification_preferences; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.user_notification_preferences ENABLE ROW LEVEL SECURITY;

--
-- Name: user_notifications; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.user_notifications ENABLE ROW LEVEL SECURITY;

--
-- Name: users; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.users ENABLE ROW LEVEL SECURITY;

--
-- Name: zone_incidents; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zone_incidents ENABLE ROW LEVEL SECURITY;

--
-- Name: zone_risk_overrides; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zone_risk_overrides ENABLE ROW LEVEL SECURITY;

--
-- Name: zz_backup_overnight2_20260710_bins; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zz_backup_overnight2_20260710_bins ENABLE ROW LEVEL SECURITY;

--
-- Name: zz_backup_overnight2_20260710_route_tasks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zz_backup_overnight2_20260710_route_tasks ENABLE ROW LEVEL SECURITY;

--
-- Name: zz_backup_overnight_20260710_bins; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zz_backup_overnight_20260710_bins ENABLE ROW LEVEL SECURITY;

--
-- Name: zz_backup_overnight_20260710_route_tasks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.zz_backup_overnight_20260710_route_tasks ENABLE ROW LEVEL SECURITY;

--
-- PostgreSQL database dump complete
--




-- +goose Down
-- Deliberately empty. "Down" from the baseline means dropping every table in the
-- database; there is no sane automatic version of that, and an accidental
-- `goose down` to version 0 would be catastrophic. Restore from a backup instead.
SELECT 1;
