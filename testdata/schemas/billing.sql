-- The northwind billing-service schema that DbProof's seed data and demo use.
CREATE TABLE public.flyway_schema_history (
  installed_rank integer NOT NULL PRIMARY KEY,
  version varchar(50),
  description varchar(200) NOT NULL,
  type varchar(20) NOT NULL,
  script varchar(1000) NOT NULL,
  checksum integer,
  installed_by varchar(100) NOT NULL,
  installed_on timestamp NOT NULL DEFAULT now(),
  execution_time integer NOT NULL,
  success boolean NOT NULL
);
CREATE INDEX flyway_schema_history_s_idx ON public.flyway_schema_history (success);

CREATE TABLE public.customers (
  id bigserial PRIMARY KEY,
  name text NOT NULL,
  email text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.invoices (
  id bigserial PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES public.customers (id),
  number varchar(32) NOT NULL UNIQUE,
  amount_cents bigint NOT NULL,
  due_date date,
  legacy_discount numeric(10, 2),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invoices_customer_id_idx ON public.invoices (customer_id);

CREATE TABLE public.payments (
  id bigserial PRIMARY KEY,
  invoice_id bigint NOT NULL REFERENCES public.invoices (id),
  customer_id bigint NOT NULL REFERENCES public.customers (id),
  amount_cents bigint NOT NULL,
  retry_count integer NOT NULL DEFAULT 0,
  next_retry_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_created_at_idx ON public.payments (created_at);

CREATE TABLE public.credit_notes (
  id bigserial PRIMARY KEY,
  invoice_id bigint NOT NULL REFERENCES public.invoices (id),
  amount_cents bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
