mod models;

use models::{Cart, Total};

pub fn run_generic<T: Total>(value: &T) -> u32 {
    value.total()
}

pub fn run_dyn(value: &dyn Total) -> u32 {
    value.total()
}

pub fn run_cart(cart: &Cart) -> u32 {
    cart.total()
}

pub fn build(count: u32) -> Cart {
    Cart { count }
}

pub fn run_generic_again<T: Total>(value: &T) -> u32 {
    value.total()
}
