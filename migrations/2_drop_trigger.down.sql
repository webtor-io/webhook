CREATE OR REPLACE FUNCTION patreon.update_members()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY patreon."member";
    RETURN NEW;
END;
$function$;

CREATE TRIGGER on_message_insert
    AFTER INSERT ON patreon.message
    FOR EACH STATEMENT
    EXECUTE FUNCTION patreon.update_members();
