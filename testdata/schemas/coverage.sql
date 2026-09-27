-- One of every object on Stratum's capture list, across two schemas. Must load
-- on every supported major (13 through 18).
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS btree_gist;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_reader') THEN CREATE ROLE app_reader NOLOGIN; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_writer') THEN CREATE ROLE app_writer NOLOGIN; END IF;
END $$;

CREATE SCHEMA app;
CREATE SCHEMA audit;

CREATE TYPE app.status AS ENUM ('draft', 'open', 'paid', 'void', 'it''s complicated');
CREATE DOMAIN app.positive_cents AS bigint NOT NULL DEFAULT 0 CHECK (VALUE >= 0);
CREATE DOMAIN app.email AS citext;
ALTER DOMAIN app.email ADD CONSTRAINT email_shape CHECK (VALUE ~ '^[^@]+@[^@]+$') NOT VALID;
CREATE TYPE app.money_pair AS (amount app.positive_cents, currency char(3));

CREATE FUNCTION app.touch_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END
$$;
CREATE FUNCTION app.cents_to_units(bigint) RETURNS numeric LANGUAGE sql IMMUTABLE AS $$ SELECT $1 / 100.0 $$;
CREATE FUNCTION app.concat_step(text, text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT coalesce($1 || ', ', '') || $2 $$;
CREATE AGGREGATE app.list(text) (SFUNC = app.concat_step, STYPE = text);

CREATE SEQUENCE app.invoice_number_seq AS integer START WITH 1000 INCREMENT BY 1 CACHE 5;

CREATE TABLE app.customers (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email app.email NOT NULL,
  name text COLLATE "C" NOT NULL,
  "Display Name" text,
  tax_id varchar(32),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT customers_email_key UNIQUE (email)
);

CREATE TABLE app.invoices (
  id serial PRIMARY KEY,
  number integer NOT NULL DEFAULT nextval('app.invoice_number_seq'),
  customer_id bigint NOT NULL REFERENCES app.customers (id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  status app.status NOT NULL DEFAULT 'draft',
  amount_cents app.positive_cents,
  amount numeric GENERATED ALWAYS AS (app.cents_to_units(amount_cents)) STORED,
  due_date date,
  legacy_discount numeric(10, 2),
  updated_at timestamptz,
  CHECK (due_date IS NULL OR due_date > '2000-01-01')
) WITH (fillfactor = 90);
ALTER SEQUENCE app.invoice_number_seq OWNED BY app.invoices.number;
ALTER TABLE app.invoices ADD CONSTRAINT invoices_amount_cap CHECK (amount_cents < 100000000) NOT VALID;
CREATE INDEX invoices_customer_idx ON app.invoices (customer_id);
CREATE INDEX invoices_open_idx ON app.invoices (due_date) WHERE status = 'open';
CREATE INDEX invoices_number_text_idx ON app.invoices ((number::text));
CREATE UNIQUE INDEX invoices_number_idx ON app.invoices (number) INCLUDE (status);
CREATE TRIGGER invoices_touch BEFORE UPDATE ON app.invoices
  FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
CREATE TRIGGER invoices_touch_insert BEFORE INSERT ON app.invoices
  FOR EACH ROW EXECUTE FUNCTION app.touch_updated_at();
ALTER TABLE app.invoices DISABLE TRIGGER invoices_touch_insert;

CREATE TABLE app.bookings (
  id bigserial PRIMARY KEY,
  room integer NOT NULL,
  during tstzrange NOT NULL,
  CONSTRAINT bookings_no_overlap EXCLUDE USING gist (room WITH =, during WITH &&)
);

CREATE TABLE app.events (
  id bigint NOT NULL,
  occurred_at timestamptz NOT NULL,
  payload jsonb DEFAULT '{}',
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);
CREATE TABLE app.events_2026 PARTITION OF app.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE app.events_default PARTITION OF app.events DEFAULT;
ALTER TABLE app.events_default ALTER COLUMN payload SET DEFAULT '{"late": true}';
CREATE INDEX events_occurred_idx ON app.events (occurred_at);
CREATE INDEX events_2026_payload_idx ON app.events_2026 USING gin (payload);

ALTER TABLE app.customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.customers FORCE ROW LEVEL SECURITY;
CREATE POLICY customers_reader ON app.customers FOR SELECT TO app_reader USING (true);
CREATE POLICY customers_writer ON app.customers AS RESTRICTIVE FOR UPDATE TO app_writer, app_reader
  USING (id > 0) WITH CHECK (id > 0);

CREATE VIEW app.open_invoices AS
  SELECT i.id, i.number, c.email, app.list(c.name) OVER () AS everyone
  FROM app.invoices i JOIN app.customers c ON c.id = i.customer_id
  WHERE i.status = 'open';
CREATE VIEW app.open_invoice_count AS SELECT count(*) AS n FROM app.open_invoices;
CREATE VIEW app.draft_invoices WITH (check_option = local, security_barrier = true) AS
  SELECT id, status FROM app.invoices WHERE status = 'draft';
CREATE MATERIALIZED VIEW app.revenue AS
  SELECT customer_id, sum(amount_cents) AS total FROM app.invoices GROUP BY customer_id;
CREATE UNIQUE INDEX revenue_customer_idx ON app.revenue (customer_id);

CREATE FUNCTION app.customer_label(c app.customers) RETURNS text LANGUAGE sql STABLE AS $$
  SELECT c.name || ' <' || c.email || '>'
$$;
CREATE PROCEDURE app.void_invoice(invoice_id integer) LANGUAGE plpgsql AS $$
BEGIN
  UPDATE app.invoices SET status = 'void' WHERE id = invoice_id;
END
$$;

CREATE UNLOGGED TABLE audit.log (
  id bigint GENERATED BY DEFAULT AS IDENTITY,
  at timestamptz DEFAULT now(),
  message text
);

GRANT USAGE ON SCHEMA app TO app_reader;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT SELECT ON app.open_invoices TO app_reader;
GRANT SELECT, INSERT ON app.invoices TO app_writer WITH GRANT OPTION;
GRANT SELECT (id, email) ON app.customers TO app_reader;
GRANT USAGE ON SEQUENCE app.invoice_number_seq TO app_writer;
REVOKE EXECUTE ON FUNCTION app.cents_to_units(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.cents_to_units(bigint) TO app_reader;
REVOKE USAGE ON TYPE app.status FROM PUBLIC;
