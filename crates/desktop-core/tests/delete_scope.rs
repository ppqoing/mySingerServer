//! 删除确认必须覆盖完整分页成员，并把同一精确集合交给节点执行。

use std::{
    net::IpAddr,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_core::{
    AnalysisRunId, ContentKey, DesktopConfig, LocationKey, MachineId, NodeEndpoint, NormalizedPath,
};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand, UiEvent},
    results::GroupKind,
    view_state::{DesktopPaths, NodeConnectionState},
};
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::proto;
use tempfile::TempDir;
use tokio::sync::oneshot;

#[derive(Clone)]
struct PagedDeleteHandler {
    machine_id: MachineId,
    first_page: Arc<Vec<proto::GroupMember>>,
    last_page: proto::GroupMember,
    executed: Arc<Mutex<Option<proto::CreateDeleteBatch>>>,
}

impl NodeRequestHandler for PagedDeleteHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: self.machine_id.as_str().into(),
                    listen_address: "127.0.0.1".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            Some(proto::envelope::Payload::ListGroupMembers(mut page)) => {
                if page.cursor == "last-page" {
                    page.members = vec![self.last_page.clone()];
                    page.next_cursor.clear();
                } else {
                    page.members = self.first_page.as_ref().clone();
                    page.next_cursor = "last-page".into();
                }
                proto::envelope::Payload::ListGroupMembers(page)
            }
            Some(proto::envelope::Payload::CreateDeleteBatch(mut batch)) => {
                *self.executed.lock().unwrap() = Some(batch.clone());
                for item in &mut batch.items {
                    item.outcome = "skipped".into();
                    item.message = "测试不执行文件操作".into();
                }
                batch.delete_batch_id = "frozen-batch".into();
                proto::envelope::Payload::CreateDeleteBatch(batch)
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "测试节点不支持该请求".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }
}

/// 破坏点：若 PrepareDelete 只使用当前 200 行窗口，本测试会少报前页 Delete/Keep，且确认集合不一致。
#[tokio::test]
async fn cross_page_confirmation_and_execution_use_the_same_complete_set() {
    let machine_id = MachineId::parse(&"d1".repeat(32)).unwrap();
    let run_id = AnalysisRunId::new();
    let group_id = "paged-group";
    let mut first_page = (0..200)
        .map(|index| member(&machine_id, index, proto::ReviewDecision::ReviewUndecided))
        .collect::<Vec<_>>();
    first_page[0].review = proto::ReviewDecision::ReviewKeep as i32;
    first_page[1].review = proto::ReviewDecision::ReviewDelete as i32;
    let last_page = member(&machine_id, 200, proto::ReviewDecision::ReviewDelete);
    let expected_locations = [
        first_page[1].location.clone().unwrap(),
        last_page.location.clone().unwrap(),
    ];
    let expected_bytes = first_page[1].content.as_ref().unwrap().file_size
        + last_page.content.as_ref().unwrap().file_size;
    let executed = Arc::new(Mutex::new(None));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        PagedDeleteHandler {
            machine_id,
            first_page: Arc::new(first_page),
            last_page,
            executed: Arc::clone(&executed),
        },
        shutdown,
    ));
    let temp = TempDir::new().unwrap();
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: IpAddr::from([127, 0, 0, 1]),
            port: address.port(),
        }],
        reconnect_interval_seconds: 60,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));

    wait_until(&mut events, |event| {
        matches!(event, UiEvent::ViewChanged(state) if state.nodes()[0].connection == NodeConnectionState::Online)
    })
    .await;
    app.send(UiCommand::LoadMembers {
        central: false,
        node_index: 0,
        analysis_run_id: run_id.as_uuid().to_string(),
        group_id: group_id.into(),
        kind: GroupKind::Exact,
        cursor: "last-page".into(),
    })
    .await
    .unwrap();
    wait_until(&mut events, |event| {
        matches!(event, UiEvent::MembersChanged { .. })
    })
    .await;
    app.send(UiCommand::PrepareDelete).await.unwrap();
    let confirmation = wait_confirmation(&mut events).await;

    assert_eq!(confirmation.file_count, 2);
    assert_eq!(confirmation.node_count, 1);
    assert_eq!(confirmation.reclaimable_bytes, expected_bytes);
    assert!(confirmation.can_execute);
    assert!(
        executed.lock().unwrap().is_none(),
        "PrepareDelete 只能读取完整分页，不能提前触发创建并立即执行的 RPC"
    );

    app.send(UiCommand::ConfirmDelete).await.unwrap();
    wait_until(&mut events, |event| {
        matches!(event, UiEvent::DeleteFinished(_))
    })
    .await;
    let request = executed.lock().unwrap().clone().unwrap();
    assert_eq!(request.analysis_run_id, run_id.as_uuid().to_string());
    assert_eq!(request.items.len(), 2);
    let actual_locations = request
        .items
        .iter()
        .map(|item| item.location.clone().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(actual_locations, expected_locations);

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}

fn member(machine_id: &MachineId, index: u16, review: proto::ReviewDecision) -> proto::GroupMember {
    let location = LocationKey::new(
        machine_id.clone(),
        NormalizedPath::new(format!(r"C:\Media\{index:03}.bin")).unwrap(),
    );
    let content = ContentKey::new([(index % 251) as u8; 16], u64::from(index) + 100);
    proto::GroupMember {
        location: Some((&location).into()),
        content: Some((&content).into()),
        review: review as i32,
        active: true,
        ..Default::default()
    }
}

async fn wait_confirmation(
    events: &mut tokio::sync::mpsc::Receiver<UiEvent>,
) -> dedup_desktop_core::delete::DeleteConfirmation {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Some(UiEvent::DeleteConfirmationChanged(confirmation)) = events.recv().await {
                break confirmation;
            }
        }
    })
    .await
    .expect("未收到删除确认摘要")
}

async fn wait_until(
    events: &mut tokio::sync::mpsc::Receiver<UiEvent>,
    mut predicate: impl FnMut(&UiEvent) -> bool,
) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Some(event) = events.recv().await
                && predicate(&event)
            {
                break;
            }
        }
    })
    .await
    .expect("等待桌面事件超时");
}

fn desktop_paths(temp: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: temp.path().to_path_buf(),
        logs: temp.path().join("logs"),
        cache: temp.path().join("cache"),
        config: temp.path().join("config.toml"),
    }
}
