package com.example.app

import com.example.core.Result
import com.example.core.capitalizeWords
import com.example.core.truncate
import com.example.models.Address
import com.example.models.Permission
import com.example.models.User
import com.example.models.UserRepository

fun main() {
    val repository = UserRepository()
    val service = UserService(repository)

    val result = service.createUser("john doe", "john@example.com", Permission.ADMIN)
    when (result) {
        is Result.Success -> {
            val user = result.value
            println("Created user: ${user.name} (${service.describeIdentifiable(user)})")
            println("Valid: ${service.validateEntity<Unit>(user)}")
        }
        is Result.Failure -> println("Error: ${result.error}")
    }

    val address = Address(
        street = "123 Main St",
        city = "springfield",
        postalCode = "12345",
        country = "US"
    )
    println("City: ${address.city.capitalizeWords()}")
    println("Street: ${address.street.truncate(10)}")

    val admins = service.listAdmins()
    println("Admins: ${admins.size}")
}
