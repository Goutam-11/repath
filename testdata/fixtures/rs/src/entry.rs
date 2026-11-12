use crate::models::User;
use crate::service::Service;
use crate::store::Store;

pub fn main_flow() -> u32 {
    let service: Service = Service::new(Store::new(), User::new(String::new()));
    service.total()
}
