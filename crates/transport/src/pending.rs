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
        self.entries
            .lock()
            .unwrap()
            .remove(&response.request_id)
            .is_some_and(|sender| sender.send(Ok(response)).is_ok())
    }

    pub(crate) fn fail(&self, request_id: u64) {
        if let Some(sender) = self.entries.lock().unwrap().remove(&request_id) {
            let _ = sender.send(Err(TransportError::ConnectionClosed));
        }
    }

    pub(crate) fn fail_all(&self) {
        for (_, sender) in self.entries.lock().unwrap().drain() {
            let _ = sender.send(Err(TransportError::ConnectionClosed));
        }
    }
}

impl Default for PendingRequests {
    fn default() -> Self {
        Self::new()
    }
}
