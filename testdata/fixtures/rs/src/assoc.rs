use crate::models::User;
use crate::service::Service;
use crate::store::Store;

pub fn build() -> u32 {
    let store = Store::new();
    let user = User::new(String::new());
    let service = Service::new(store, user);
    service.total()
}
