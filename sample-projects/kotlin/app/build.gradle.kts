plugins {
    application
}

dependencies {
    implementation(project(":core"))
    implementation(project(":models"))
}

application {
    mainClass.set("com.example.app.ApplicationKt")
}
