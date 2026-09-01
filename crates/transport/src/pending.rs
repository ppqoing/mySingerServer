//! 并发请求按非零 request_id 等待和分派响应。

use std::{collections::HashMap, sync::Mutex};

use dedup_protocol::proto;
use tokio::sync::oneshot;

use crate::TransportError;

pub(crate) type PendingResponse = oneshot::Receiver<Result<proto::Envelope, TransportError>>;

pub(crate) struct PendingRequests {
    entries: Mutex<HashMap<u64, oneshot::Sender<Result<proto::Envelope, TransportError>>>>,
}

impl PendingRequests {
    pub(crate) fn new() -> Self {
        Self {
            entries: Mutex::new(HashMap::new()),
        }
    }

    pub(crate) fn register(&self, request_id: u64) -> PendingResponse {
        let (sender, receiver) = oneshot::channel();
        self.entries.lock().unwrap().insert(request_id, sender);
        receiver
    }

    pub(crate) fn resolve(&self, response: proto::Envelope) -> bool {
        let request_id = response.request_id;
        let Some(sender) = self.entries.lock().unwrap().remove(&request_id) else {
            return false;
        };
        match sender.send(Ok(response)) {
            Ok(()) => true,
            Err(unsent_response) => {
                drop(unsent_response);
                tracing::info!(
                    event = "expected_condition",
                    component = "transport_pending",
                    operation = "resolve_request",
                    reason = "request_receiver_closed",
                    request_id,
                    error = "oneshot_receiver_closed",
                    "响应到达前请求调用方已经结束"
                );
                false
            }
        }
    }

    pub(crate) fn fail(&self, request_id: u64) {
        if let Some(sender) = self.entries.lock().unwrap().remove(&request_id) {
            if let Err(unsent_result) = sender.send(Err(TransportError::ConnectionClosed)) {
                drop(unsent_result);
                tracing::info!(
                    event = "expected_condition",
                    component = "transport_pending",
                    operation = "fail_request",
                    reason = "request_receiver_closed",
                    request_id,
                    error = "oneshot_receiver_closed",
                    "等待请求结果的调用方已经结束"
                );
            }
        }
    }

    pub(crate) fn fail_all(&self) {
        for (request_id, sender) in self.entries.lock().unwrap().drain() {
            if let Err(unsent_result) = sender.send(Err(TransportError::ConnectionClosed)) {
                drop(unsent_result);
                tracing::info!(
                    event = "expected_condition",
                    component = "transport_pending",
                    operation = "fail_all_requests",
                    reason = "request_receiver_closed",
                    request_id,
                    error = "oneshot_receiver_closed",
                    "等待请求结果的调用方已经结束"
                );
            }
        }
    }
}

impl Default for PendingRequests {
    fn default() -> Self {
        Self::new()
    }
}
