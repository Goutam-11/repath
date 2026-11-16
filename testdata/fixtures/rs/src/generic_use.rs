use crate::service::Describe;

pub fn show<T: Describe>(value: T) -> u32 {
    value.describe()
}
