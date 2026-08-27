//! Desktop 与 Node 共用的 PostgreSQL 存储公开边界。

use dedup_central_store::{CentralError, CentralStore};

#[test]
fn central_store_is_available_without_desktop_core() {
    fn accepts(_: Option<CentralStore>, _: Option<CentralError>) {}

    accepts(None, None);
}
