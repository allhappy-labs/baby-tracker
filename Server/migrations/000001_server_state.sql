CREATE TABLE server_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);
