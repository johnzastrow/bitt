-- 0009_tab_reminders: per-tab, Provider-set payment reminders.
--
-- Phase 5 shipped reminders as instance-wide environment configuration
-- (BITT_REMINDER_DAYS, BITT_REMINDER_TITLE/BODY). That was always the fallback
-- layer: the Provider owns a tab's billing cadence, so the Provider is who
-- should say when its payees are reminded and what the message says. This table
-- is that override, in the same shape the fee and interest settings already
-- use -- a tab with no rows here falls back to the instance defaults, and
-- clearing the rows returns it to them.
--
-- One row per lead time, mirroring the instance list, so a tab can remind at
-- several distances from a due date. The title and body live on each row rather
-- than on the tab, which is what lets a "one day left" message differ from a
-- "two weeks out" one later without another migration; today the interface
-- writes the same pair to every row of a tab, which is exactly how the instance
-- default behaves.
--
-- This table is CONFIGURATION, not a claim: unlike posted_periods,
-- sent_notifications and their siblings it is meant to be edited, so it carries
-- no append-only triggers. Rewriting a reminder cannot re-send anything --
-- delivery is claimed per (tab, event, channel) in sent_notifications, and that
-- key does not include the message text.
--
-- A NOTE ON TRUST. Until now these templates were admin text from the
-- environment. A Provider is an ordinary user, so the title and body here are
-- USER-controlled text that reaches a mail header ({tab} in a Subject). The
-- handler validates both on the way in -- no control characters in a title, and
-- only newlines in a body -- and internal/notify still REJECTS control
-- characters in every header value at send time. Two layers, and the second one
-- fails the send closed rather than injecting a header.
CREATE TABLE tab_reminders (
    tab_id  INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    -- days is how many days before the due date the reminder fires. Positive
    -- and bounded, matching the validation on BITT_REMINDER_DAYS.
    days    INTEGER NOT NULL CHECK (days > 0 AND days <= 3650),
    title   TEXT    NOT NULL,
    body    TEXT    NOT NULL,
    PRIMARY KEY (tab_id, days)
);
