use std::path::Path;

use node::{TrayAction, TrayCommand, TrayState};

#[test]
fn tray_commands_map_to_one_engine_action_and_single_shutdown() {
    let mut state = TrayState::new(Path::new(r"C:\Portable\data\node\logs"));

    assert_eq!(
        state.apply(TrayCommand::OpenLogs),
        Some(TrayAction::OpenLogs(
            Path::new(r"C:\Portable\data\node\logs").to_path_buf()
        ))
    );
    assert_eq!(
        state.apply(TrayCommand::RestartEngine),
        Some(TrayAction::RestartEngine)
    );
    assert_eq!(state.apply(TrayCommand::Exit), Some(TrayAction::Shutdown));
    assert_eq!(state.apply(TrayCommand::Exit), None);
}
