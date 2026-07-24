-- 0010_instance_notify: instance-wide notification settings an administrator
-- can set through the interface.
--
-- WHAT IS AND IS NOT HERE, and why the line falls where it does.
--
-- Here: the NON-SECRET delivery settings and the default reminder messages.
-- These are ordinary configuration -- a hostname, a port, an address, some
-- message text -- and there is no reason an administrator should need shell
-- access to a container to change the wording of a reminder.
--
-- NOT here, deliberately: the SMTP password, the ntfy token, and the tick
-- secret. Those stay in the environment or a file (`BITT_SMTP_PASSWORD`,
-- `BITT_NTFY_TOKEN`, `BITT_TICK_SECRET`, each accepting `file:`). A secret in
-- this table would have to be encrypted at rest, the key for that would have to
-- come from the environment anyway, and the net result is the same number of
-- environment secrets plus a key to manage and re-wrap. It would also put live
-- credentials into every backup taken under DEPLOY-07. The tick secret has a
-- further reason: it is what makes /internal/tick fail closed, and "unset
-- refuses everything" is a posture worth keeping out of reach of a session.
--
-- PRECEDENCE: the environment wins. A setting present in the environment is
-- authoritative and the interface shows it read-only; the columns here apply
-- only where the environment is silent. That keeps a container deployment
-- reproducible from its compose file while still giving a bare-binary install a
-- way to configure itself.
--
-- Empty string and 0 mean "not set here", matching every other optional column
-- in this schema -- no NULLs to COALESCE (DEPLOY-02).
ALTER TABLE instance ADD COLUMN smtp_host     TEXT    NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN smtp_port     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE instance ADD COLUMN smtp_username TEXT    NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN email_from    TEXT    NOT NULL DEFAULT '';
ALTER TABLE instance ADD COLUMN ntfy_url      TEXT    NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------
-- instance_reminders: the default reminder set, when the environment does not
-- specify one.
--
-- Same shape as tab_reminders (migration 0009), one level up, and the same
-- resolution order applies at send time: a tab's own reminders, then these,
-- then the built-in 14/7/1. Configuration rather than a claim, so no
-- append-only triggers -- editing a message cannot re-send anything, because
-- sent_notifications is keyed on (tab, event, channel) and the message text is
-- nowhere in that key.
-- ---------------------------------------------------------------------------
CREATE TABLE instance_reminders (
    days  INTEGER NOT NULL PRIMARY KEY CHECK (days > 0 AND days <= 3650),
    title TEXT    NOT NULL,
    body  TEXT    NOT NULL
);
