-- Migration 0033: Integrate Mabar Sessions dengan Open Play Pendaftaran
-- Purpose: Link existing mabar_sessions ke open_play_sessions
--          Sync attendances ke open_play_participants
--          Allow status changes via Open Play registration UI

-- Step 1: Add column to mabar_sessions to reference open_play_sessions
ALTER TABLE mabar_sessions
ADD COLUMN IF NOT EXISTS open_play_session_id UUID REFERENCES open_play_sessions(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_mabar_sessions_open_play_session_id 
ON mabar_sessions(open_play_session_id);

-- Step 2: Add column to attendances to track sync status
ALTER TABLE attendances
ADD COLUMN IF NOT EXISTS synced_to_open_play BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_attendances_synced_to_open_play 
ON attendances(synced_to_open_play);

-- Step 3: Add view to get mabar session details with open play info
CREATE OR REPLACE VIEW v_mabar_with_open_play AS
SELECT 
  ms.id as mabar_session_id,
  ms.period_id,
  ms.venue_id,
  ms.date,
  ms.start_time,
  ms.end_time,
  ms.type as mabar_type,
  ms.status as mabar_status,
  ops.id as open_play_session_id,
  ops.name as open_play_name,
  ops.schedule_date,
  ops.start_time as open_play_start_time,
  ops.max_members,
  ops.max_non_members,
  ops.status as open_play_status,
  ops.id IS NOT NULL as has_open_play
FROM mabar_sessions ms
LEFT JOIN open_play_sessions ops ON ms.open_play_session_id = ops.id;

-- Step 4: Create helper function to sync mabar to open_play
CREATE OR REPLACE FUNCTION sync_mabar_to_open_play(
  p_mabar_session_id UUID,
  p_open_play_session_id UUID
)
RETURNS TABLE (
  synced_count INT,
  error_message TEXT
) AS $$
DECLARE
  v_error TEXT;
  v_synced_count INT := 0;
BEGIN
  BEGIN
    -- Sync all attendances with status 'LISTED' to open_play_participants
    INSERT INTO open_play_participants (
      id, session_id, player_id, participant_type, participant_status, joined_at
    )
    SELECT 
      gen_random_uuid(),
      p_open_play_session_id,
      a.player_id,
      CASE WHEN a.is_member THEN 'MEMBER' ELSE 'NON_MEMBER' END,
      'REGISTERED',
      COALESCE(a.listed_at, now())
    FROM attendances a
    WHERE a.session_id = p_mabar_session_id
      AND a.status = 'LISTED'
      AND NOT EXISTS (
        SELECT 1 FROM open_play_participants opp
        WHERE opp.session_id = p_open_play_session_id
          AND opp.player_id = a.player_id
      )
    ON CONFLICT DO NOTHING;
    
    GET DIAGNOSTICS v_synced_count = ROW_COUNT;
    
    -- Mark attendances as synced
    UPDATE attendances 
    SET synced_to_open_play = TRUE
    WHERE session_id = p_mabar_session_id
      AND synced_to_open_play = FALSE;
    
    RETURN QUERY SELECT v_synced_count, NULL::TEXT;
    
  EXCEPTION WHEN OTHERS THEN
    v_error := SQLERRM;
    RETURN QUERY SELECT 0::INT, v_error;
  END;
END;
$$ LANGUAGE plpgsql;

-- Step 5: Create helper function to update mabar status from open_play
CREATE OR REPLACE FUNCTION update_mabar_from_open_play(
  p_participant_id UUID,
  p_new_status TEXT
)
RETURNS TABLE (
  updated BOOLEAN,
  error_message TEXT
) AS $$
DECLARE
  v_session_id UUID;
  v_mabar_session_id UUID;
  v_player_id UUID;
  v_error TEXT;
BEGIN
  BEGIN
    -- Get participant details
    SELECT opp.session_id, opp.player_id INTO v_session_id, v_player_id
    FROM open_play_participants opp
    WHERE opp.id = p_participant_id;
    
    IF v_session_id IS NULL THEN
      RETURN QUERY SELECT FALSE, 'Participant not found'::TEXT;
      RETURN;
    END IF;
    
    -- Get mabar session linked to this open_play session
    SELECT ms.id INTO v_mabar_session_id
    FROM mabar_sessions ms
    WHERE ms.open_play_session_id = v_session_id;
    
    IF v_mabar_session_id IS NULL THEN
      RETURN QUERY SELECT FALSE, 'No mabar session linked'::TEXT;
      RETURN;
    END IF;
    
    -- Map open_play status to attendance status
    UPDATE attendances
    SET status = CASE 
          WHEN p_new_status = 'REGISTERED' THEN 'LISTED'
          WHEN p_new_status = 'WAITING' THEN 'WAITING'
          WHEN p_new_status = 'CANCELLED' THEN 'CANCELLED'
          WHEN p_new_status = 'AUTO_FILLED' THEN 'LISTED'
          ELSE 'LISTED'
        END,
        updated_at = now()
    WHERE session_id = v_mabar_session_id
      AND player_id = v_player_id;
    
    RETURN QUERY SELECT TRUE, NULL::TEXT;
    
  EXCEPTION WHEN OTHERS THEN
    v_error := SQLERRM;
    RETURN QUERY SELECT FALSE, v_error;
  END;
END;
$$ LANGUAGE plpgsql;

-- Step 6: Add comment for future reference
COMMENT ON TABLE mabar_sessions IS 
'Existing mabar sessions. Can be linked to open_play_sessions via open_play_session_id.
When linked, attendances are synced to open_play_participants for registration management.';

COMMENT ON COLUMN mabar_sessions.open_play_session_id IS
'Link to open_play_sessions for registration-based mabar (New flow).
When set, members can change attendance status via Open Play registration UI.';

COMMENT ON FUNCTION sync_mabar_to_open_play IS
'Sync existing mabar attendances to open_play_participants.
Used when converting a mabar session to use the new Open Play registration flow.';

COMMENT ON FUNCTION update_mabar_from_open_play IS
'Update mabar attendance status when changed via Open Play registration UI.
Keeps both systems in sync.';
